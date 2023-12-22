// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package providers

import (
	"crypto/sha256"
	"fmt"
	"io"
	"sync"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
)

// functionResultsCache is a global cache to verify the pure-ness of all
// provider implemented functions.
var functionResultsCache = newFunctionResults()

type FunctionDecl struct {
	Parameters        []FunctionParam
	VariadicParameter *FunctionParam
	ReturnType        cty.Type

	Description     string
	DescriptionKind configschema.StringKind
}

type FunctionParam struct {
	Name string // Only for documentation and UI, because arguments are positional
	Type cty.Type

	AllowNullValue     bool
	AllowUnknownValues bool

	Description     string
	DescriptionKind configschema.StringKind
}

// BuildFunction takes a factory function which will return an unconfigured
// instance of the provider this declaration belongs to and returns a
// cty function that is ready to be called against that provider.
//
// The given name must be the name under which the provider originally
// registered this declaration, or the returned function will try to use an
// invalid name, leading to errors or undefined behavior.
//
// If the given factory returns an instance of any provider other than the
// one the declaration belongs to, or returns a _configured_ instance of
// the provider rather than an unconfigured one, the behavior of the returned
// function is undefined.
//
// Although not functionally required, callers should ideally pass a factory
// function that either retrieves already-running plugins or memoizes the
// plugins it returns so that many calls to functions in the same provider
// will not incur a repeated startup cost.
func (d FunctionDecl) BuildFunction(providerAddr addrs.Provider, name string, factory func() (Interface, error)) function.Function {

	var params []function.Parameter
	var varParam *function.Parameter
	if len(d.Parameters) > 0 {
		params = make([]function.Parameter, len(d.Parameters))
		for i, paramDecl := range d.Parameters {
			params[i] = paramDecl.ctyParameter()
		}
	}
	if d.VariadicParameter != nil {
		cp := d.VariadicParameter.ctyParameter()
		varParam = &cp
	}

	return function.New(&function.Spec{
		Type:     function.StaticReturnType(d.ReturnType),
		Params:   params,
		VarParam: varParam,
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			for i, arg := range args {
				var param function.Parameter
				if i < len(params) {
					param = params[i]
				} else {
					param = *varParam
				}

				// We promise provider developers that we won't pass them even
				// _nested_ unknown values unless they opt in to dealing with
				// them.
				if !param.AllowUnknown {
					if !arg.IsWhollyKnown() {
						return cty.UnknownVal(retType), nil
					}
				}

				// We also ensure that null values are never passed where they
				// are not expected.
				if !param.AllowNull {
					if arg.IsNull() {
						return cty.UnknownVal(retType), fmt.Errorf("argument %q cannot be null", param.Name)
					}
				}
			}

			provider, err := factory()
			if err != nil {
				return cty.UnknownVal(retType), fmt.Errorf("failed to launch provider plugin: %s", err)
			}

			resp := provider.CallFunction(CallFunctionRequest{
				FunctionName: name,
				Arguments:    args,
			})
			// NOTE: We don't actually have any way to surface warnings
			// from the function here, because functions just return normal
			// Go errors rather than diagnostics.
			if resp.Diagnostics.HasErrors() {
				return cty.UnknownVal(retType), resp.Diagnostics.Err()
			}

			if resp.Result == cty.NilVal {
				return cty.UnknownVal(retType), fmt.Errorf("provider returned no result and no errors")
			}

			err = functionResultsCache.checkPrior(providerAddr, name, args, resp.Result)
			if err != nil {
				return cty.UnknownVal(retType), err
			}

			return resp.Result, nil
		},
	})
}

func (p *FunctionParam) ctyParameter() function.Parameter {
	return function.Parameter{
		Name:      p.Name,
		Type:      p.Type,
		AllowNull: p.AllowNullValue,

		// While the function may not allow DynamicVal, a `null` literal is
		// also dynamically typed. If the parameter is dynamically typed, then
		// we must allow this for `null` to pass through.
		AllowDynamicType: p.Type == cty.DynamicPseudoType,

		// NOTE: Setting this is not a sufficient implementation of
		// FunctionParam.AllowUnknownValues, because cty's function
		// system only blocks passing in a top-level unknown, but
		// our provider-contributed functions API promises to only
		// pass wholly-known values unless AllowUnknownValues is true.
		// The function implementation itself must also check this.
		AllowUnknown: p.AllowUnknownValues,
	}
}

type priorResult struct {
	hash [sha256.Size]byte
	// when the result was from a current run, we keep a record of the result
	// value to aid in debugging. Results stored in the plan will only have the
	// hash to avoid bloating the plan with what could be many very large
	// values.
	value cty.Value
}

type functionResults struct {
	mu sync.Mutex
	// results stores the prior result from a provider function call, keyed by
	// the hash of the function name and arguments.
	results map[[sha256.Size]byte]priorResult
}

func newFunctionResults() *functionResults {
	return &functionResults{
		results: make(map[[sha256.Size]byte]priorResult),
	}
}

// checkPrior compares the function call against any cached results, and
// returns an error if the result does not match a prior call.
func (f *functionResults) checkPrior(provider addrs.Provider, name string, args []cty.Value, result cty.Value) error {
	argSum := sha256.New()

	io.WriteString(argSum, provider.String())
	io.WriteString(argSum, "|"+name)

	for _, arg := range args {
		// cty.Values have a Hash method, but it is not collision resistant. We
		// are going to rely on the GoString formatting instead, which gives
		// detailed results for all values.
		io.WriteString(argSum, "|"+arg.GoString())
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	argHash := [sha256.Size]byte(argSum.Sum(nil))
	resHash := sha256.Sum256([]byte(result.GoString()))

	res, ok := f.results[argHash]
	if !ok {
		f.results[argHash] = priorResult{
			hash:  resHash,
			value: result,
		}
		return nil
	}

	// FIXME: We don't have marks at this point, so we can't skip sensitive
	// values. We may not be able to provide the result value for debugging.
	if resHash != res.hash {
		// The hcl package will add the necessary context around the error in
		// the diagnostic, but we add the differing results when we can.
		// TODO: maybe we should add a call to action, since this is a bug in
		//       the provider.
		if res.value != cty.NilVal {
			return fmt.Errorf("Provider function returned an inconsistent result,\nwas: %#v,\nnow: %#v", res.value, result)

		}
		return fmt.Errorf("Provider function returned an inconsistent result.")
	}

	return nil
}

// add inserts a new key-value pair to the functionResults map. This is used to
// preload stored values before any Verify calls are made.
func (f *functionResults) add(argHash, resHash [sha256.Size]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.results[argHash]; ok {
		return
	}
	f.results[argHash] = priorResult{hash: resHash}
}
