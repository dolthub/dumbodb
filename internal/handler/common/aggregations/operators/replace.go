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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// replaceOp represents { $replaceOne: { input, find, replacement } } and the
// same for $replaceAll.
type replaceOp struct {
	opName                   string
	input, find, replacement any
}

// newReplace follows parseExpressionReplaceBase.
func newReplace(opName string) func(args ...any) (Operator, error) {
	return func(args ...any) (Operator, error) {
		op := &replaceOp{opName: opName}
		spec := argObject{
			notObject: func(arg any) error {
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrReplaceNotObject,
					fmt.Sprintf("%s requires an object as an argument, found: %s", opName, handlerparams.AliasFromType(arg)), opName)
			},
			unknown: func(key string) error {
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrReplaceUnknownField,
					fmt.Sprintf("%s found an unknown argument: %s", opName, key), opName)
			},
			fields: []argField{
				{name: "input", set: assign(&op.input, nil), missing: handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrReplaceNoInput, opName+" requires 'input' to be specified", opName)},
				{name: "find", set: assign(&op.find, nil), missing: handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrReplaceNoFind, opName+" requires 'find' to be specified", opName)},
				{name: "replacement", set: assign(&op.replacement, nil), missing: handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrReplaceNoReplacement, opName+" requires 'replacement' to be specified", opName)},
			},
		}
		if err := spec.parse(args); err != nil {
			return nil, err
		}
		return op, nil
	}
}

// Process follows ExpressionReplaceBase::evaluate: every operand is evaluated
// and type checked before a nullish one makes the result null.
func (op *replaceOp) Process(doc *types.Document) (any, error) {
	var values [3]any
	for i, arg := range []any{op.input, op.find, op.replacement} {
		v, err := evalArgValue(arg, doc)
		if err != nil {
			return nil, err
		}
		values[i] = v
	}

	codes := [3]handlererrors.ErrorCode{
		handlererrors.ErrReplaceInputType, handlererrors.ErrReplaceFindType, handlererrors.ErrReplaceReplacementType,
	}
	for i, name := range []string{"input", "find", "replacement"} {
		if _, ok := values[i].(string); !ok && !isNullish(values[i]) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(codes[i],
				fmt.Sprintf("%s requires that '%s' be a string, found: %s", op.opName, name, types.FormatAnyValue(values[i])), op.opName)
		}
	}
	for _, v := range values {
		if isNullish(v) {
			return types.Null, nil
		}
	}

	input, find, replacement := values[0].(string), values[1].(string), values[2].(string)
	matches := 1
	switch {
	case op.opName == "$replaceOne" && !strings.Contains(input, find):
		matches = 0
	case op.opName == "$replaceAll" && find == "":
		matches = len(input) + 1
	case op.opName == "$replaceAll":
		matches = strings.Count(input, find)
	}
	if err := checkStringResultLength(len(input) + matches*(len(replacement)-len(find))); err != nil {
		return nil, err
	}

	if op.opName == "$replaceOne" {
		return strings.Replace(input, find, replacement, 1), nil
	}
	return replaceAllBytes(input, find, replacement), nil
}

// Limits on a string an expression builds, as mongod applies them:
// BufBuilder cannot grow past BufferMaxSize, and a string value must be
// smaller than BSONObjMaxUserSize.
const (
	bufferMaxSize      = 125 * 1024 * 1024
	bsonObjMaxUserSize = 16 * 1024 * 1024
)

// checkStringResultLength returns mongod's error for a string result of n
// bytes, checked before the string is built.
func checkStringResultLength(n int) error {
	switch {
	case n > bufferMaxSize:
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrBufBuilderTooLarge,
			fmt.Sprintf("BufBuilder attempted to grow() to %d bytes, past the %dMB limit.", n, bufferMaxSize/(1024*1024)))
	case n >= bsonObjMaxUserSize:
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrStringTooLarge,
			fmt.Sprintf("RCString too large. Requires size=%d < limit=%d", n, bsonObjMaxUserSize))
	}
	return nil
}

// replaceAllBytes is strings.ReplaceAll, except that an empty find inserts
// replacement between bytes rather than between UTF-8 sequences, as MongoDB
// does.
func replaceAllBytes(input, find, replacement string) string {
	if find != "" {
		return strings.ReplaceAll(input, find, replacement)
	}
	var b strings.Builder
	for i := 0; i < len(input); i++ {
		b.WriteString(replacement)
		b.WriteByte(input[i])
	}
	b.WriteString(replacement)
	return b.String()
}

var _ Operator = (*replaceOp)(nil)
