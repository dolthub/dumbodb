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
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func EvalArgValue(arg any, doc *types.Document) (any, error) {
	return evalArgValue(arg, doc)
}

// EvalOperand evaluates any aggregation expression, reporting whether it
// produced MongoDB's missing value (an absent field or $$REMOVE). As in
// MongoDB, a document literal omits fields that are missing and an array
// literal holds null in their place.
func EvalOperand(expr any, doc *types.Document) (value any, missing bool, err error) {
	switch v := expr.(type) {
	case *types.Document:
		if IsOperator(v) {
			op, err := cachedOperator(v)
			if err != nil {
				return nil, false, err
			}
			value, err = op.Process(doc)
			if errors.Is(err, aggregations.ErrMissingValue) {
				return nil, true, nil
			}
			return value, false, withOperatorContext(err, v.Command())
		}
		value, err = evalDocumentExpressions(v, doc)
		return value, false, err

	case *types.Array:
		out := types.MakeArray(v.Len())
		for i := range v.Len() {
			elem, elemMissing, err := EvalOperand(must.NotFail(v.Get(i)), doc)
			if err != nil {
				return nil, false, err
			}
			if elemMissing {
				elem = types.Null
			}
			out.Append(elem)
		}
		return out, false, nil

	case string:
		if v == "$$REMOVE" {
			return nil, true, nil
		}
		value, missing, err = evalArgValueOrMissing(v, doc)
		if errors.Is(err, aggregations.ErrMissingValue) {
			return nil, true, nil
		}
		return value, missing, err

	default:
		return v, false, nil
	}
}

// evalBranch evaluates an expression an operator returns as is, such as a
// $cond branch, so that a missing value stays missing (ErrMissingValue).
func evalBranch(arg any, doc *types.Document) (any, error) {
	v, missing, err := EvalOperand(arg, doc)
	if err != nil {
		return nil, err
	}
	if missing {
		return nil, aggregations.ErrMissingValue
	}
	return v, nil
}

// missingVariablePrefix marks a variable bound to the missing value. Variable
// names cannot start with '$', so the key cannot collide with a binding.
const missingVariablePrefix = "$$$missing:"

// bindVariable binds $$name in doc to v, or to the missing value, replacing
// any outer binding of the same name.
func bindVariable(doc *types.Document, name string, v any, missing bool) {
	if missing {
		doc.Remove("$$" + name)
		doc.Set(missingVariablePrefix+name, true)
		return
	}
	doc.Remove(missingVariablePrefix + name)
	doc.Set("$$"+name, v)
}

// isMissingVariable reports whether doc binds $$name to the missing value.
func isMissingVariable(doc *types.Document, name string) bool {
	return doc != nil && doc.Has(missingVariablePrefix+name)
}

// IsConstantExpression reports whether expr reads no field and no variable,
// so that mongod folds it to a constant while parsing.
func IsConstantExpression(expr any) bool {
	return isConstantExpression(expr)
}

func isConstantExpression(expr any) bool {
	switch v := expr.(type) {
	case string:
		// mongod folds $$REMOVE to the missing value.
		return !strings.HasPrefix(v, "$") || v == "$$REMOVE"
	case *types.Array:
		for i := range v.Len() {
			if !isConstantExpression(must.NotFail(v.Get(i))) {
				return false
			}
		}
		return true
	case *types.Document:
		if IsOperator(v) {
			if bindsVariables(v) {
				return false
			}
			switch v.Command() {
			case "$literal", "$const":
				return !isBoundNow(v)
			case "$rand", "$sampleRate", "$meta":
				return false
			case "$getField":
				if getFieldReadsCurrent(v) {
					return false
				}
			}
		}
		for _, k := range v.Keys() {
			if !isConstantExpression(must.NotFail(v.Get(k))) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// bindsVariables reports whether an operator binds a variable, which keeps
// mongod from folding it: $filter, $map, $reduce, and $let with variables.
func bindsVariables(op *types.Document) bool {
	switch op.Command() {
	case "$filter", "$map", "$reduce":
		return true
	case "$let":
		spec, _ := must.NotFail(op.Get("$let")).(*types.Document)
		if spec == nil {
			return false
		}
		v, _ := spec.Get("vars")
		vars, _ := v.(*types.Document)
		return vars != nil && vars.Len() > 0
	}
	return false
}

// getFieldReadsCurrent reports whether a $getField expression has no input,
// so that it reads $$CURRENT.
func getFieldReadsCurrent(expr *types.Document) bool {
	spec, ok := must.NotFail(expr.Get("$getField")).(*types.Document)
	return !ok || !isFullForm(spec) || !spec.Has("input")
}

// checkConstantOperand returns the error mongod raises when it folds a
// constant operand that fails while parsing, even if the operand would
// never be evaluated.
func checkConstantOperand(expr any) error {
	if !isConstantExpression(expr) {
		return nil
	}
	_, _, err := foldConstant(expr, nil)
	return err
}

// convertOperators are left unfolded: mongod's $project and find
// projections do not report a failing constant conversion while parsing.
var convertOperators = map[string]bool{
	"$convert": true, "$toBool": true, "$toDate": true, "$toDecimal": true, "$toDouble": true,
	"$toInt": true, "$toLong": true, "$toObjectId": true, "$toString": true, "$toUUID": true,
}

// CheckConstantSubexpressions returns the error mongod raises while parsing
// when it folds a constant operator anywhere inside expr. Like mongod's
// optimize, it folds children first, so a failing constant is an error even
// in a branch never taken, and each operator is evaluated once, over its
// children's folded values.
func CheckConstantSubexpressions(expr any) error {
	_, _, err := foldConstants(expr)
	return err
}

// foldConstants returns expr with each folded operator replaced by
// {$literal: value}, and whether expr is constant.
func foldConstants(expr any) (any, bool, error) {
	switch v := expr.(type) {
	case string:
		return v, isConstantExpression(v), nil
	case *types.Array:
		folded := types.MakeArray(v.Len())
		constant := true
		for i := range v.Len() {
			elem, elemConstant, err := foldConstants(must.NotFail(v.Get(i)))
			if err != nil {
				return nil, false, err
			}
			folded.Append(elem)
			constant = constant && elemConstant
		}
		return folded, constant, nil
	case *types.Document:
		return foldDocumentConstants(v)
	default:
		return v, true, nil
	}
}

func foldDocumentConstants(doc *types.Document) (any, bool, error) {
	operator := IsOperator(doc)
	name := ""
	if operator {
		name = doc.Command()
		// Unknown names are not expressions here, such as $sortArray's
		// sortBy {"$a": 1}; inside a real expression parsing rejects them.
		if _, known := Operators[name]; !known || name == "$literal" || name == "$const" {
			return doc, isConstantExpression(doc), nil
		}
	}

	if name == "$switch" {
		return foldSwitchConstants(doc)
	}

	folded := types.MakeDocument(doc.Len())
	constant := true
	keys, values := doc.Keys(), doc.Values()
	for i, value := range values {
		value, valueConstant, err := foldConstants(value)
		if err != nil {
			return nil, false, err
		}
		folded.AppendDecoded(keys[i], value)
		constant = constant && valueConstant
	}

	switch {
	case !operator:
		return folded, constant, nil
	case name == "$rand" || name == "$sampleRate" || name == "$meta" || bindsVariables(doc):
		return folded, false, nil
	case name == "$getField" && getFieldReadsCurrent(doc):
		return folded, false, nil
	case !constant || convertOperators[name]:
		return folded, constant, nil
	}

	value, missing, err := foldConstant(folded, nil)
	if err != nil {
		return nil, false, err
	}
	if missing {
		return folded, false, nil
	}
	return must.NotFail(types.NewDocument("$literal", value)), true, nil
}

// foldSwitchConstants folds a $switch as mongod's optimizer does: the default
// first, then each case in turn. A constant falsy case drops its branch
// unfolded, and a constant truthy one makes its then the default and drops
// every later branch. With no branch left and no default it fails.
func foldSwitchConstants(doc *types.Document) (any, bool, error) {
	arg := must.NotFail(doc.Get("$switch"))
	if _, err := newSwitch(arg); err != nil {
		return nil, false, err
	}
	spec := arg.(*types.Document)

	var def any
	var defConstant, hasDefault bool
	if v, err := spec.Get("default"); err == nil {
		if def, defConstant, err = foldConstants(v); err != nil {
			return nil, false, err
		}
		hasDefault = true
	}

	branches := must.NotFail(spec.Get("branches")).(*types.Array)
	kept := types.MakeArray(branches.Len())
	for i := range branches.Len() {
		branch := must.NotFail(branches.Get(i)).(*types.Document)
		caseExpr, caseConstant, err := foldConstants(must.NotFail(branch.Get("case")))
		if err != nil {
			return nil, false, err
		}
		if caseConstant {
			v, _, err := foldConstant(caseExpr, nil)
			if err != nil {
				return nil, false, err
			}
			if isFalsy(v) {
				continue
			}
			if def, defConstant, err = foldConstants(must.NotFail(branch.Get("then"))); err != nil {
				return nil, false, err
			}
			hasDefault = true
			break
		}
		thenExpr, _, err := foldConstants(must.NotFail(branch.Get("then")))
		if err != nil {
			return nil, false, err
		}
		kept.Append(must.NotFail(types.NewDocument("case", caseExpr, "then", thenExpr)))
	}

	if kept.Len() == 0 {
		if !hasDefault {
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSwitchNoMatch,
				"Cannot execute a switch statement where all the cases evaluate to false without a default", "$switch")
		}
		return def, defConstant, nil
	}

	folded := must.NotFail(types.NewDocument("branches", kept))
	if hasDefault {
		folded.Set("default", def)
	}
	return must.NotFail(types.NewDocument("$switch", folded)), false, nil
}

// foldConstant is EvalOperand. Operator constructors call it, and EvalOperand
// reaches Operators, so it is bound in init to avoid an initialization cycle.
var foldConstant func(expr any, doc *types.Document) (any, bool, error)

func init() {
	foldConstant = EvalOperand
}

// copyForVariables copies doc so an operator can bind variables in it.
// Expressions are validated against no document at all (nil), which binds
// variables like a document with no fields.
func copyForVariables(doc *types.Document) *types.Document {
	if doc == nil {
		return types.MakeDocument(0)
	}
	return doc.DeepCopy()
}

// evalArgValue evaluates an operand. A missing field, or an operator that
// produces the missing value, reads as null, as MongoDB's operators treat
// missing operands as nullish; $$REMOVE alone is reported as ErrMissingValue.
func evalArgValue(arg any, doc *types.Document) (any, error) {
	switch v := arg.(type) {
	case *types.Document:
		if IsOperator(v) {
			op, err := cachedOperator(v)
			if err != nil {
				return nil, err
			}

			res, err := op.Process(doc)
			if errors.Is(err, aggregations.ErrMissingValue) {
				return types.Null, nil
			}
			return res, err
		}

		return evalDocumentExpressions(v, doc)

	case *types.Array:
		// An array literal is an expression: each element is evaluated, and a
		// missing element becomes null.
		out := types.MakeArray(v.Len())
		for i := range v.Len() {
			elem, err := evalArgValue(must.NotFail(v.Get(i)), doc)
			if errors.Is(err, aggregations.ErrMissingValue) {
				elem, err = types.Null, nil
			}
			if err != nil {
				return nil, err
			}
			out.Append(elem)
		}
		return out, nil

	case string:
		if strings.HasPrefix(v, "$$") {
			withoutPrefix := strings.TrimPrefix(v, "$$")
			varName := withoutPrefix
			fieldPath := ""
			if dotIdx := strings.Index(withoutPrefix, "."); dotIdx >= 0 {
				varName = withoutPrefix[:dotIdx]
				fieldPath = withoutPrefix[dotIdx+1:]
			}

			if varName == "" {
				return nil, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrFailedToParse,
					"empty variable names are not allowed",
				)
			}

			var base any
			switch varName {
			case "ROOT", "CURRENT":
				base = copyForVariables(doc)
			case "NOW":
				base = time.Now().UTC()
			case "REMOVE":
				// An operator reads the missing value as null; $cond and the
				// stages read it with EvalOperand, which keeps it missing.
				return types.Null, nil
			case "IS_MR", "CLUSTER_TIME":
				return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBuiltinVariableUnavailable,
					"Builtin variable '$$"+varName+"' is not available")
			case "USER_ROLES":
				base = types.MakeArray(0)
			case "PRUNE", "KEEP", "DESCEND":
				// $redact reads these as literal strings to decide its action.
				base = v
			default:
				if isMissingVariable(doc, varName) {
					return types.Null, nil
				}
				val, err := doc.Get("$$" + varName)
				if err != nil {
					return nil, handlererrors.NewCommandErrorMsg(
						handlererrors.ErrGroupUndefinedVariable,
						"Use of undefined variable: "+varName,
					)
				}
				base = val
			}

			if fieldPath == "" {
				return base, nil
			}

			baseDoc, ok := base.(*types.Document)
			if !ok {
				return types.Null, nil
			}

			path, err := types.NewPathFromString(fieldPath)
			if err != nil {
				return types.Null, nil
			}

			result, err := baseDoc.GetByPath(path)
			if err != nil {
				return types.Null, nil
			}

			return result, nil
		}

		if strings.HasPrefix(v, "$") {
			expr, err := fieldExpression(v)
			if err != nil {
				var exErr *aggregations.ExpressionError
				if errors.As(err, &exErr) && exErr.Code() == aggregations.ErrNotExpression {
					return v, nil
				}

				return nil, err
			}

			val, err := expr.Evaluate(doc)
			if err != nil {
				return types.Null, nil
			}

			return val, nil
		}

		return v, nil

	default:
		return v, nil
	}
}

// evalDocumentExpressions evaluates a document literal as MongoDB's
// ExpressionObject does: a field whose value is missing is omitted.
func evalDocumentExpressions(expr *types.Document, doc *types.Document) (*types.Document, error) {
	result := types.MakeDocument(expr.Len())

	for _, k := range expr.Keys() {
		evaluated, missing, err := EvalOperand(must.NotFail(expr.Get(k)), doc)
		if err != nil {
			return nil, err
		}
		if !missing {
			result.Set(k, evaluated)
		}
	}

	return result, nil
}

// isNullish reports whether v is null or undefined, which expressions treat
// alike.
func isNullish(v any) bool {
	return v == types.Null || v == types.Undefined
}

// isFalsy reports whether v is false in a boolean context.
func isFalsy(v any) bool {
	return !coerceToBool(v)
}

// coerceToBool is Value::coerceToBool: false, numeric zeros, null,
// undefined and missing are false; everything else is true.
func coerceToBool(v any) bool {
	switch v.(type) {
	case nil, types.NullType, types.UndefinedType:
		return false
	default:
		return convertToBool(v)
	}
}

// OperatorContextError records the innermost operator an evaluation error
// came from, for errors whose code alone does not say.
type OperatorContextError struct {
	Op  string
	Err error
}

func (e *OperatorContextError) Error() string { return e.Err.Error() }

func (e *OperatorContextError) Unwrap() error { return e.Err }

// WithOperatorContext returns err marked as raised by the operator op, unless
// it already names an operator.
func WithOperatorContext(err error, op string) error {
	return withOperatorContext(err, op)
}

func withOperatorContext(err error, op string) error {
	var ctxErr *OperatorContextError
	if err == nil || errors.As(err, &ctxErr) {
		return err
	}
	return &OperatorContextError{Op: op, Err: err}
}
