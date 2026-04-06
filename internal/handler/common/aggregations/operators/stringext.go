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
	"strings"

	"github.com/dolthub/docudolt/internal/types"
)

// ── $strcasecmp ───────────────────────────────────────────────────────────────

// strcasecmpOp represents $strcasecmp.
//
//	{ $strcasecmp: [ <expression1>, <expression2> ] }
type strcasecmpOp struct {
	lhs any
	rhs any
}

// newStrcasecmp creates a new $strcasecmp operator.
func newStrcasecmp(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$strcasecmp",
			fmt.Sprintf("Expression $strcasecmp takes exactly 2 arguments. %d were passed in.", len(args)),
		)
	}

	return &strcasecmpOp{lhs: args[0], rhs: args[1]}, nil
}

// Process implements Operator. Returns -1, 0, or 1 for case-insensitive string comparison.
func (op *strcasecmpOp) Process(doc *types.Document) (any, error) {
	av, err := evalArgValue(op.lhs, doc)
	if err != nil {
		return nil, err
	}

	bv, err := evalArgValue(op.rhs, doc)
	if err != nil {
		return nil, err
	}

	a := ""
	b := ""

	if av != types.Null {
		if s, ok := av.(string); ok {
			a = s
		}
	}

	if bv != types.Null {
		if s, ok := bv.(string); ok {
			b = s
		}
	}

	al := strings.ToLower(a)
	bl := strings.ToLower(b)

	switch {
	case al < bl:
		return int32(-1), nil
	case al > bl:
		return int32(1), nil
	default:
		return int32(0), nil
	}
}

// check interfaces
var _ Operator = (*strcasecmpOp)(nil)

// ── $substrBytes ─────────────────────────────────────────────────────────────

// substrBytesOp represents $substrBytes.
//
//	{ $substrBytes: [ <string>, <start>, <length> ] }
type substrBytesOp struct {
	str    any
	start  any
	length any
}

// newSubstrBytesOp creates a new $substrBytes operator.
func newSubstrBytesOp(args ...any) (Operator, error) {
	if len(args) != 3 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$substrBytes",
			fmt.Sprintf("Expression $substrBytes takes exactly 3 arguments. %d were passed in.", len(args)),
		)
	}

	return &substrBytesOp{str: args[0], start: args[1], length: args[2]}, nil
}

// Process implements Operator. Returns substring by byte offsets.
func (op *substrBytesOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.str, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return "", nil
	}

	s, ok := sv.(string)
	if !ok {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$substrBytes",
			fmt.Sprintf("$substrBytes requires a string first argument, got %T", sv),
		)
	}

	startV, err := evalArgValue(op.start, doc)
	if err != nil {
		return nil, err
	}

	lenV, err := evalArgValue(op.length, doc)
	if err != nil {
		return nil, err
	}

	start := int(toFloat64(startV))
	length := int(toFloat64(lenV))

	b := []byte(s)

	if start < 0 {
		start = 0
	}

	if start >= len(b) {
		return "", nil
	}

	b = b[start:]

	if length < 0 {
		return string(b), nil
	}

	if length > len(b) {
		length = len(b)
	}

	return string(b[:length]), nil
}

// check interfaces
var _ Operator = (*substrBytesOp)(nil)
