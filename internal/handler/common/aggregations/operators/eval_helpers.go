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
			return value, false, err
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

// isConstantExpression reports whether expr reads no field and no variable,
// so that mongod folds it to a constant while parsing.
func isConstantExpression(expr any) bool {
	switch v := expr.(type) {
	case string:
		return !strings.HasPrefix(v, "$")
	case *types.Array:
		for i := range v.Len() {
			if !isConstantExpression(must.NotFail(v.Get(i))) {
				return false
			}
		}
		return true
	case *types.Document:
		if IsOperator(v) {
			switch v.Command() {
			case "$literal":
				return true
			case "$rand", "$sampleRate", "$meta":
				return false
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
// projections do not report a failing constant conversion while parsing
// (workspace-2vz).
var convertOperators = map[string]bool{
	"$convert": true, "$toBool": true, "$toDate": true, "$toDecimal": true, "$toDouble": true,
	"$toInt": true, "$toLong": true, "$toObjectId": true, "$toString": true, "$toUUID": true,
}

// CheckConstantSubexpressions returns the error mongod raises while parsing
// when it folds a constant operator anywhere inside expr, children first, so
// a failing constant is an error even in a branch never taken.
func CheckConstantSubexpressions(expr any) error {
	switch v := expr.(type) {
	case *types.Array:
		for i := range v.Len() {
			if err := CheckConstantSubexpressions(must.NotFail(v.Get(i))); err != nil {
				return err
			}
		}
	case *types.Document:
		operator := IsOperator(v)
		if operator && v.Command() == "$literal" {
			return nil
		}
		for _, value := range v.Values() {
			if err := CheckConstantSubexpressions(value); err != nil {
				return err
			}
		}
		if operator && !convertOperators[v.Command()] && isConstantExpression(v) {
			_, _, err := foldConstant(v, nil)
			return err
		}
	}
	return nil
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
				// Missing rather than null, so a $cond selecting $$REMOVE drops
				// the field instead of writing an explicit null.
				return nil, aggregations.ErrMissingValue
			case "PRUNE", "KEEP", "DESCEND":
				// $redact reads these as literal strings to decide its action.
				base = v
			default:
				if isMissingVariable(doc, varName) {
					return nil, aggregations.ErrMissingValue
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

// isFalsy returns true when v is MongoDB-falsy for $cond purposes:
// false (bool), null, or missing.
func isFalsy(v any) bool {
	switch val := v.(type) {
	case bool:
		return !val
	case types.NullType, types.UndefinedType:
		return true
	default:
		_ = val
		return false
	}
}
