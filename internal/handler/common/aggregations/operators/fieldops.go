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
	"errors"
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// ErrMissingValue is returned by an operator whose result is MongoDB's "missing"
// value. Callers must omit the target field entirely rather than storing null;
// $project already does the same for field paths that resolve to nothing.
//
// It aliases the expression-level sentinel so that a missing value raised by an
// operator and one raised by $$REMOVE compare equal under errors.Is.
var ErrMissingValue = aggregations.ErrMissingValue

// removeVariable suppresses a field when used as a $setField value. It is
// matched before evaluation because evalArgValue collapses it to null.
const removeVariable = "$$REMOVE"

// FieldOpErrorCode maps a field-operator construction failure to the MongoDB
// error code for it, reporting false for any other error. Stages use it to
// surface the operator's own identity instead of a generic stage error.
func FieldOpErrorCode(err error) (handlererrors.ErrorCode, bool) {
	var opErr OperatorError
	if !errors.As(err, &opErr) {
		return 0, false
	}

	switch opErr.Code() {
	case ErrGetFieldUnknownArgument:
		return handlererrors.ErrGetFieldUnknownArgument, true
	case ErrGetFieldMissingField:
		return handlererrors.ErrGetFieldMissingField, true
	case ErrGetFieldMissingInput:
		return handlererrors.ErrGetFieldMissingInput, true
	case ErrSetFieldUnknownArgument:
		return handlererrors.ErrSetFieldUnknownArgument, true
	case ErrSetFieldMissingField:
		return handlererrors.ErrSetFieldMissingField, true
	case ErrSetFieldMissingInput:
		return handlererrors.ErrSetFieldMissingInput, true
	case ErrSetFieldMissingValue:
		return handlererrors.ErrSetFieldMissingValue, true
	case ErrSetFieldFieldNotString:
		return handlererrors.ErrSetFieldFieldNotString, true
	default:
		return 0, false
	}
}

// aliasOrMissing names a value for the "but got <type>" error messages,
// reporting MongoDB's "missing" for a value that does not exist.
func aliasOrMissing(v any, present bool) string {
	if !present {
		return "missing"
	}

	return handlerparams.AliasFromType(v)
}

// evalFieldExpr evaluates a 'field' argument. The bool result is false when the
// expression resolves to missing, which MongoDB reports separately from null.
func evalFieldExpr(arg any, doc *types.Document) (any, bool, error) {
	s, ok := arg.(string)
	if !ok || !strings.HasPrefix(s, "$") || strings.HasPrefix(s, "$$") {
		v, err := evalArgValue(arg, doc)
		if err != nil {
			return nil, false, err
		}

		return v, true, nil
	}

	expr, err := aggregations.NewExpression(s, nil)
	if err != nil {
		var exErr *aggregations.ExpressionError
		if errors.As(err, &exErr) && exErr.Code() == aggregations.ErrNotExpression {
			return s, true, nil
		}

		return nil, false, err
	}

	v, err := expr.Evaluate(doc)
	if err != nil {
		return nil, false, nil
	}

	return v, true, nil
}

// resolveInput evaluates an 'input' argument to the document the operator reads
// or writes. The bool result is false when input is not a document, which
// $getField reports as missing rather than as an error.
func resolveInput(arg any, doc *types.Document) (*types.Document, bool, error) {
	if arg == nil {
		return doc, true, nil
	}

	v, err := evalArgValue(arg, doc)
	if err != nil {
		return nil, false, err
	}

	in, ok := v.(*types.Document)
	if !ok {
		return nil, false, nil
	}

	return in, true, nil
}

// fieldOpCodes carries the per-operator error identities. $getField and the
// $setField family report the same conditions under different codes.
type fieldOpCodes struct {
	unknownArg   operatorErrorCode
	missingField operatorErrorCode
	missingInput operatorErrorCode
}

var (
	getFieldCodes = fieldOpCodes{ErrGetFieldUnknownArgument, ErrGetFieldMissingField, ErrGetFieldMissingInput}
	setFieldCodes = fieldOpCodes{ErrSetFieldUnknownArgument, ErrSetFieldMissingField, ErrSetFieldMissingInput}
)

// fieldOpArgs holds the parsed arguments common to the three field operators.
type fieldOpArgs struct {
	field any
	input any
	value any
}

// parseFieldOpArgs reads the full-form argument document, rejecting arguments
// the operator does not accept. wantValue enables the $setField 'value' key.
// All three operators require both 'field' and 'input' in this form.
func parseFieldOpArgs(doc *types.Document, opName string, codes fieldOpCodes, wantValue bool) (*fieldOpArgs, error) {
	out := new(fieldOpArgs)
	var hasField, hasInput, hasValue bool

	for _, k := range doc.Keys() {
		v := must.NotFail(doc.Get(k))

		switch {
		case k == "field":
			out.field = v
			hasField = true
		case k == "input":
			out.input = v
			hasInput = true
		case k == "value" && wantValue:
			out.value = v
			hasValue = true
		default:
			return nil, newOperatorError(codes.unknownArg, opName,
				fmt.Sprintf("%s found an unknown argument: %s", opName, k))
		}
	}

	switch {
	case !hasField:
		return nil, newOperatorError(codes.missingField, opName,
			fmt.Sprintf("%s requires 'field' to be specified", opName))
	case !hasInput:
		return nil, newOperatorError(codes.missingInput, opName,
			fmt.Sprintf("%s requires 'input' to be specified", opName))
	case wantValue && !hasValue:
		return nil, newOperatorError(ErrSetFieldMissingValue, opName,
			fmt.Sprintf("%s requires 'value' to be specified", opName))
	}

	return out, nil
}

// isFullForm reports whether the operator argument is an argument document
// rather than a bare field-name expression.
func isFullForm(v any) bool {
	doc, ok := v.(*types.Document)
	return ok && !IsOperator(doc)
}

// getFieldOp implements {$getField: <field>} and
// {$getField: {field: <expr>, input: <expr>}}.
//
// The operator reads exactly one key: it performs no dot-path descent and does
// not map over arrays, which is what makes it able to reach keys whose names
// contain dots or begin with a dollar sign.
type getFieldOp struct {
	field any
	input any
}

func newGetField(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$getField",
			fmt.Sprintf("Expression $getField takes exactly 1 argument. %d were passed in.", len(args)))
	}

	if !isFullForm(args[0]) {
		return &getFieldOp{field: args[0]}, nil
	}

	parsed, err := parseFieldOpArgs(args[0].(*types.Document), "$getField", getFieldCodes, false)
	if err != nil {
		return nil, err
	}

	return &getFieldOp{field: parsed.field, input: parsed.input}, nil
}

func (op *getFieldOp) Process(doc *types.Document) (any, error) {
	if doc == nil {
		return types.Null, nil
	}

	name, present, err := evalFieldExpr(op.field, doc)
	if err != nil {
		return nil, err
	}

	key, ok := name.(string)
	if !ok {
		// MongoDB validates the $getField field type when the expression runs,
		// unlike $setField which rejects it while parsing.
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrGetFieldFieldNotString,
			fmt.Sprintf("$getField requires 'field' to evaluate to type String, but got %s",
				aliasOrMissing(name, present)),
		)
	}

	in := doc

	if op.input != nil {
		v, err := evalArgValue(op.input, doc)
		if err != nil {
			return nil, err
		}

		// A null input reads back as null, while any other non-document input
		// yields missing.
		if _, isNull := v.(types.NullType); isNull {
			return types.Null, nil
		}

		if in, ok = v.(*types.Document); !ok {
			return nil, ErrMissingValue
		}
	}

	if !in.Has(key) {
		return nil, ErrMissingValue
	}

	return must.NotFail(in.Get(key)), nil
}

// setFieldOp implements {$setField: {field: <expr>, input: <expr>, value: <expr>}}
// and, with remove set, {$unsetField: {field: <expr>, input: <expr>}}.
type setFieldOp struct {
	field  any
	input  any
	value  any
	name   string
	opName string
	remove bool
}

func newSetField(args ...any) (Operator, error) {
	return newFieldMutator(args, "$setField", false)
}

func newUnsetField(args ...any) (Operator, error) {
	return newFieldMutator(args, "$unsetField", true)
}

// newFieldMutator builds $setField and $unsetField. MongoDB reports both under
// the $setField name when 'field' is not a string, which the parity suite pins.
func newFieldMutator(args []any, opName string, remove bool) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, opName,
			fmt.Sprintf("Expression %s takes exactly 1 argument. %d were passed in.", opName, len(args)))
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrSetFieldFieldNotString, opName,
			fmt.Sprintf("$setField requires 'field' to evaluate to type String, but got %s",
				handlerparams.AliasFromType(args[0])))
	}

	parsed, err := parseFieldOpArgs(doc, opName, setFieldCodes, !remove)
	if err != nil {
		return nil, err
	}

	op := &setFieldOp{
		field:  parsed.field,
		input:  parsed.input,
		value:  parsed.value,
		opName: opName,
		remove: remove,
	}

	// A constant field name is rejected while parsing, matching MongoDB.
	if name, ok := parsed.field.(string); ok && !strings.HasPrefix(name, "$") {
		op.name = name
		return op, nil
	}

	if !isFieldExpression(parsed.field) {
		return nil, newOperatorError(ErrSetFieldFieldNotString, opName,
			fmt.Sprintf("$setField requires 'field' to evaluate to type String, but got %s",
				handlerparams.AliasFromType(parsed.field)))
	}

	return op, nil
}

// isFieldExpression reports whether a 'field' argument can only be resolved by
// evaluating it against a document.
func isFieldExpression(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.HasPrefix(t, "$")
	case *types.Document:
		return IsOperator(t)
	default:
		return false
	}
}

// Process applies the mutation. A nil document means the operator is being
// probed by stage validation, where the real result is not yet knowable.
func (op *setFieldOp) Process(doc *types.Document) (any, error) {
	if doc == nil {
		return types.Null, nil
	}

	key := op.name

	if key == "" {
		name, present, err := evalFieldExpr(op.field, doc)
		if err != nil {
			return nil, err
		}

		s, ok := name.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrSetFieldFieldNotString,
				fmt.Sprintf("$setField requires 'field' to evaluate to type String, but got %s",
					aliasOrMissing(name, present)),
			)
		}

		key = s
	}

	in, ok, err := resolveInput(op.input, doc)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, ErrMissingValue
	}

	result := in.DeepCopy()

	if op.remove || op.value == removeVariable {
		result.Remove(key)
		return result, nil
	}

	value, err := evalArgValue(op.value, doc)
	if err != nil {
		return nil, err
	}

	result.Set(key, value)

	return result, nil
}

var (
	_ Operator = (*getFieldOp)(nil)
	_ Operator = (*setFieldOp)(nil)
)
