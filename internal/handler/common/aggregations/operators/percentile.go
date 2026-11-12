// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operators

import (
	"fmt"
	"math"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// PercentileSpec is a parsed $percentile or $median specification. $median
// has the single percentile 0.5.
type PercentileSpec struct {
	Input any
	Ps    []float64
}

// ParsePercentileSpec parses the specification v of the $percentile or $median
// accumulator or expression name, reporting a non-object v with notObject.
func ParsePercentileSpec(name string, median bool, v any, notObject handlererrors.ErrorCode) (*PercentileSpec, error) {
	spec, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			notObject, fmt.Sprintf("specification must be an object; found %s: %s", name, types.FormatAnyValue(v)), name,
		)
	}
	var input, p, method any
	for _, key := range spec.Keys() {
		v := must.NotFail(spec.Get(key))
		switch {
		case key == "input":
			input = v
		case key == "p" && !median:
			p = v
		case key == "method":
			method = v
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrIDLUnknownField, fmt.Sprintf("BSON field '%s.%s' is an unknown field.", name, key), name,
			)
		}
	}
	missing := func(field string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrMissingField, fmt.Sprintf("BSON field '%s.%s' is missing but a required field", name, field), name,
		)
	}
	switch {
	case input == nil:
		return nil, missing("input")
	case p == nil && !median:
		return nil, missing("p")
	case method == nil:
		return nil, missing("method")
	}
	if _, ok := method.(string); !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf("BSON field '%s.method' is the wrong type '%s', expected type 'string'", name, handlerparams.AliasFromType(method)),
			name,
		)
	}
	if method != "approximate" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "Currently only 'approximate' can be used as percentile 'method'.", name,
		)
	}

	res := &PercentileSpec{Input: input, Ps: []float64{0.5}}
	if !median {
		var err error
		if res.Ps, err = parsePercentiles(p); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// evalConstant evaluates a constant expression. It is set in init because
// operator constructors cannot refer to the operator table statically.
var evalConstant func(expr any) (any, error)

func init() {
	evalConstant = func(expr any) (any, error) {
		v, _, err := EvalOperand(expr, types.MakeDocument(0))
		return v, err
	}
}

// parsePercentiles returns the constant percentiles of p.
func parsePercentiles(p any) ([]float64, error) {
	if !isConstantExpression(p) || hasUnfoldedOperator(p) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrPercentileNotConstant,
			"The $percentile 'p' field must be an array of constant values, but found value: "+types.FormatAnyValue(p)+".",
			"$percentile",
		)
	}
	v, err := evalConstant(p)
	if err != nil {
		return nil, err
	}
	invalid := func(code handlererrors.ErrorCode, found any) error {
		return handlererrors.NewCommandErrorMsgWithArgument(
			code,
			"The $percentile 'p' field must be an array of numbers from [0.0, 1.0], but found: "+types.FormatAnyValue(found),
			"$percentile",
		)
	}
	arr, ok := v.(*types.Array)
	if !ok || arr.Len() == 0 {
		return nil, invalid(handlererrors.ErrPercentileNotArray, v)
	}
	ps := make([]float64, 0, arr.Len())
	for i := range arr.Len() {
		elem := must.NotFail(arr.Get(i))
		f, ok := numericValue(elem)
		if !ok {
			return nil, invalid(handlererrors.ErrPercentileNotNumber, elem)
		}
		if f < 0 || f > 1 || math.IsNaN(f) {
			return nil, invalid(handlererrors.ErrPercentileOutOfRange, elem)
		}
		ps = append(ps, f)
	}
	return ps, nil
}

// hasUnfoldedOperator reports whether v uses an operator that mongod does not
// fold into a constant even when its arguments are constant.
func hasUnfoldedOperator(v any) bool {
	switch v := v.(type) {
	case *types.Array:
		for i := range v.Len() {
			if hasUnfoldedOperator(must.NotFail(v.Get(i))) {
				return true
			}
		}
	case *types.Document:
		switch v.Command() {
		case "$literal":
			return false
		case "$map", "$filter", "$sortArray", "$zip", "$reduce", "$let":
			return true
		}
		for _, k := range v.Keys() {
			if hasUnfoldedOperator(must.NotFail(v.Get(k))) {
				return true
			}
		}
	}
	return false
}

// Percentile returns the value at rank ceil(p*n) of the sorted values, mongod's
// "approximate" method, or null when there are none.
func Percentile(sorted []float64, p float64) any {
	if len(sorted) == 0 {
		return types.Null
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}
