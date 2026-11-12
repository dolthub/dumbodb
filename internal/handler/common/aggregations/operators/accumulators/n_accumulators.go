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

package accumulators

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// nArg is the 'n' of an N accumulator, evaluated once per group where no
// document field is available. mongod's classic engine validates it then;
// its SBE engine validates a constant n while it builds the plan and reports
// errors under its own codes.
type nArg struct {
	expr any
	kind nKind
	sbe  bool
	op   string
}

type nKind int

const (
	nConstant nKind = iota
	nFieldPath
	nRoot
)

func newNArg(expr any, op string) nArg {
	return nArg{expr: expr, kind: classifyN(expr), op: op}
}

// classifyN reports what n reads: nothing that varies by document, a field,
// or the whole document.
func classifyN(expr any) nKind {
	kind := nConstant
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			switch {
			case v == "$$ROOT" || v == "$$CURRENT":
				kind = nRoot
			case kind == nConstant && (strings.HasPrefix(v, "$$ROOT.") || strings.HasPrefix(v, "$$CURRENT.") ||
				strings.HasPrefix(v, "$") && !strings.HasPrefix(v, "$$")):
				kind = nFieldPath
			}
		case *types.Array:
			for i := range v.Len() {
				walk(must.NotFail(v.Get(i)))
			}
		case *types.Document:
			if v.Command() == "$literal" {
				return
			}
			for _, k := range v.Keys() {
				walk(must.NotFail(v.Get(k)))
			}
		}
	}
	walk(expr)
	return kind
}

// useSBE switches n to SBE's validation, reporting an invalid constant n.
func (a *nArg) useSBE() error {
	a.sbe = true
	if a.kind != nConstant {
		return nil
	}
	_, err := a.resolve()
	return err
}

func (a *nArg) resolve() (int64, error) {
	v, missing, err := operators.EvalOperand(a.expr, types.MakeDocument(0))
	if err != nil {
		return 0, err
	}
	if !a.sbe {
		if missing {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNOperatorNNotIntegral, "Value for 'n' must be of integral type, but found MISSING", "n",
			)
		}
		return operators.IntegralN(v, "n")
	}
	if n, err := operators.IntegralN(v, "n"); err == nil && !missing {
		return n, nil
	}
	switch a.kind {
	case nRoot:
		// SBE reads the whole document as $firstN's state, and checks it
		// earlier for the others.
		code := handlererrors.ErrNAccumulatorRootState
		if a.op == "$firstN" {
			code = handlererrors.ErrNAccumulatorStateNotArray
		}
		return 0, handlererrors.NewCommandErrorMsgWithArgument(code, "The accumulator state should be an array", "n")
	case nFieldPath:
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNAccumulatorNInvalidAtRuntime, "parameter 'n' must be coercible to a positive 64-bit integer", "n",
		)
	}
	return 0, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrNAccumulatorNInvalid, "parameter 'n' must be coercible to a positive 64-bit integer", "n",
	)
}

func specNotObjectError(code handlererrors.ErrorCode, op string, v any) error {
	return handlererrors.NewCommandErrorMsgWithArgument(
		code, fmt.Sprintf("specification must be an object; found %s: %s", op, types.FormatAnyValue(v)), op,
	)
}

// nAccumulator implements $firstN, $lastN, $minN and $maxN.
type nAccumulator struct {
	name         string
	input        operand
	n            nArg
	stopWhenFull bool
}

func newNAccumulatorFunc(name string) newAccumulatorFunc {
	return func(args ...any) (Accumulator, error) {
		spec, ok := args[0].(*types.Document)
		if !ok {
			return nil, specNotObjectError(handlererrors.ErrNAccumulatorSpecNotObject, name, args[0])
		}
		var input, n any
		for _, key := range spec.Keys() {
			switch key {
			case "input":
				input = must.NotFail(spec.Get(key))
			case "n":
				n = must.NotFail(spec.Get(key))
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrNAccumulatorUnknownArgument, "Unknown argument for 'n' operator: "+key, name,
				)
			}
		}
		if n == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNAccumulatorMissingN, "Missing value for 'n'", name,
			)
		}
		if input == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNAccumulatorMissingInput, "Missing value for 'input'", name,
			)
		}
		inputOperand, err := newOperand(input)
		if err != nil {
			return nil, err
		}
		return &nAccumulator{name: name, input: inputOperand, n: newNArg(n, name)}, nil
	}
}

// UseSBE implements SBEAccumulator.
func (a *nAccumulator) UseSBE() error {
	return a.n.useSBE()
}

func (a *nAccumulator) New() Accumulation {
	return &nState{spec: a}
}

type nState struct {
	spec   *nAccumulator
	values []any
	ranked *rankedSet
	n      int64
	ready  bool
	mem    keptMemory
	all    bool
}

func (s *nState) keepAll() { s.all = true }

func (s *nState) Accumulate(doc *types.Document) error {
	if !s.ready {
		n, err := s.spec.n.resolve()
		if err != nil {
			return err
		}
		s.n, s.ready = n, true
		switch s.spec.name {
		case "$minN":
			s.ranked = newRankedSet(n, compareValues, false, s.spec.n.sbe)
		case "$maxN":
			s.ranked = newRankedSet(n, compareValues, true, s.spec.n.sbe)
		}
		if s.ranked != nil {
			s.ranked.all = s.all
		}
	}
	full := s.spec.name == "$firstN" && int64(len(s.values)) >= s.n
	if full && s.spec.stopWhenFull {
		return nil
	}
	v, missing, err := s.spec.input.evaluate(doc)
	if err != nil || full {
		return err
	}
	if s.ranked != nil {
		if !missing && !isNull(v) {
			return s.ranked.add(v, v, &s.mem, s.spec.name)
		}
		return nil
	}
	if missing {
		v = types.Null
	}
	s.values = append(s.values, v)
	if s.spec.name == "$lastN" && int64(len(s.values)) > s.n {
		s.mem.release(s.values[0])
		s.values = s.values[1:]
	}
	return s.mem.keep(v, s.spec.name)
}

func (s *nState) Result() (any, error) {
	values := s.values
	if s.ranked != nil {
		switch {
		case s.ranked.sbe:
			values = s.ranked.sortedSBE(s.spec.name == "$maxN")
		default:
			values = s.ranked.sorted()
			if s.spec.name == "$maxN" {
				slices.Reverse(values)
			}
		}
	}
	out := types.MakeArray(len(values))
	for _, v := range values {
		out.Append(v)
	}
	return out, nil
}

func compareValues(a, b any) int {
	return int(types.CompareOrder(a, b, types.Ascending))
}

func isNull(v any) bool {
	_, ok := v.(types.NullType)
	return ok
}

var (
	_ Accumulator  = (*nAccumulator)(nil)
	_ Accumulation = (*nState)(nil)
)
