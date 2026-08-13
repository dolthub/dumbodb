// Copyright 2021 FerretDB Inc.
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

package aggregations

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// ErrMissingValue reports MongoDB's "missing" value, which callers render by
// omitting the field rather than storing null. $$REMOVE evaluates to it.
var ErrMissingValue = errors.New("expression produced a missing value")

// System variables resolvable without a surrounding variable scope. Names
// introduced by $let and friends stay undefined.
const (
	varRoot    = "ROOT"
	varCurrent = "CURRENT"
	varNow     = "NOW"
	varRemove  = "REMOVE"
)

//go:generate ../../../../bin/stringer -linecomment -type ExpressionErrorCode

// ExpressionErrorCode represents Expression error code.
type ExpressionErrorCode int

const (
	_ ExpressionErrorCode = iota
	ErrNotExpression
	ErrInvalidExpression
	ErrEmptyFieldPath
	ErrUndefinedVariable
	ErrEmptyVariable
)

// ExpressionError describes an error that occurs while evaluating expression.
type ExpressionError struct {
	code ExpressionErrorCode
	name string
}

func newExpressionError(code ExpressionErrorCode, name string) error {
	return &ExpressionError{code: code, name: name}
}

func (e *ExpressionError) Error() string {
	return e.code.String()
}

func (e *ExpressionError) Code() ExpressionErrorCode {
	return e.code
}

// Name returns the value of an expression that produced an error.
// For an expression `$$$variable`, the invalid value `$variable` is set.
func (e *ExpressionError) Name() string {
	return e.name
}

// Expression represents a value that needs evaluation.
//
// Expression for access field in document should be prefixed with a dollar sign $ followed by field key.
// For accessing embedded document or array, a dollar sign $ should be followed by dot notation.
// Options can be provided to specify how to access fields in embedded array.
type Expression struct {
	opts commonpath.FindValuesOpts
	path types.Path

	// variable is the system variable name for a $$-prefixed expression, empty
	// otherwise. When set, path holds the suffix reaching into it and may be
	// empty, as in "$$ROOT" against "$$ROOT.a".
	variable string
	varPath  string
}

// NewExpression returns Expression from dollar sign $ prefixed string.
// It can take additional options to specify how to access fields in embedded array.
//
// It returns error if invalid Expression is provided.
func NewExpression(expression string, opts *commonpath.FindValuesOpts) (*Expression, error) {
	// for aggregation expression, it does not return value by index of array
	if opts == nil {
		opts = &commonpath.FindValuesOpts{
			FindArrayIndex:     false,
			FindArrayDocuments: true,
		}
	}

	var val string

	switch {
	case strings.HasPrefix(expression, "$$"):
		// double dollar sign $$ prefixed string indicates Expression is a variable name
		v := strings.TrimPrefix(expression, "$$")
		if v == "" {
			return nil, newExpressionError(ErrEmptyVariable, v)
		}

		if strings.HasPrefix(v, "$") {
			return nil, newExpressionError(ErrInvalidExpression, v)
		}

		name, suffix := v, ""
		if dot := strings.Index(v, "."); dot >= 0 {
			name, suffix = v[:dot], v[dot+1:]
		}

		switch name {
		case varRoot, varCurrent, varNow, varRemove:
			return &Expression{opts: *opts, variable: name, varPath: suffix}, nil
		}

		return nil, newExpressionError(ErrUndefinedVariable, v)
	case strings.HasPrefix(expression, "$"):
		// dollar sign $ prefixed string indicates Expression accesses field or embedded fields
		val = strings.TrimPrefix(expression, "$")

		if val == "" {
			return nil, newExpressionError(ErrEmptyFieldPath, val)
		}
	default:
		return nil, newExpressionError(ErrNotExpression, val)
	}

	var err error

	path, err := types.NewPathFromString(val)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return &Expression{
		path: path,
		opts: *opts,
	}, nil
}

// Evaluate uses Expression to find a field value or an embedded field value of the document and
// returns found value. If values were found from embedded array, it returns *types.Array
// containing values.
//
// It returns error if field value was not found. With embedded array field being exception,
// that case it returns empty array instead of error.
func (e *Expression) Evaluate(doc *types.Document) (any, error) {
	if e.variable != "" {
		return e.evaluateVariable(doc)
	}

	path := e.path

	if path.Len() == 1 {
		val, err := doc.Get(path.String())
		if err != nil {
			return nil, err
		}

		return val, nil
	}

	var isArrayField bool
	prefix := path.Prefix()

	if v, err := doc.Get(prefix); err == nil {
		if _, isArray := v.(*types.Array); isArray {
			isArrayField = true
		}
	}

	vals, err := commonpath.FindValues(doc, path, &e.opts)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if len(vals) == 0 {
		if isArrayField {
			// embedded array field returns empty array
			return must.NotFail(types.NewArray()), nil
		}

		return nil, fmt.Errorf("no document found under %s path", path)
	}

	if len(vals) == 1 && !isArrayField {
		// when it is not an embedded array field, return the value
		return vals[0], nil
	}

	// embedded array field returns an array of found values
	arr := types.MakeArray(len(vals))
	for _, v := range vals {
		arr.Append(v)
	}

	return arr, nil
}

// GetExpressionSuffix returns field key of Expression, or for dot notation it returns suffix.
// evaluateVariable resolves a system variable, applying varPath when the
// expression reached into it. Callers treat an error as missing, which is how
// $$REMOVE drops its field.
func (e *Expression) evaluateVariable(doc *types.Document) (any, error) {
	var base any

	switch e.variable {
	case varRoot, varCurrent:
		base = doc.DeepCopy()
	case varNow:
		return time.Now().UTC(), nil
	case varRemove:
		return nil, ErrMissingValue
	default:
		return nil, newExpressionError(ErrUndefinedVariable, e.variable)
	}

	if e.varPath == "" {
		return base, nil
	}

	path, err := types.NewPathFromString(e.varPath)
	if err != nil {
		return nil, ErrMissingValue
	}

	v, err := base.(*types.Document).GetByPath(path)
	if err != nil {
		return nil, ErrMissingValue
	}

	return v, nil
}

func (e *Expression) GetExpressionSuffix() string {
	return e.path.Suffix()
}

// GetExpressionPath returns the full path of the Expression.
func (e *Expression) GetExpressionPath() types.Path {
	return e.path
}
