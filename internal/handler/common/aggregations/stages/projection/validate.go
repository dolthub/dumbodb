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

package projection

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// context selects whose projection rules a specification is validated by.
type context int

const (
	// stageContext is $project's: no positional paths or projection
	// operators, and errors prefixed "Invalid $project".
	stageContext context = iota
	// findContext is find's and findAndModify's: positional paths and the
	// $elemMatch, $slice and $meta projection operators are allowed, and an
	// empty projection is valid.
	findContext
)

const stageErrorPrefix = "Invalid $project :: caused by :: "

func (c context) prefix() string {
	if c == findContext {
		return ""
	}
	return stageErrorPrefix
}

// argument is the error argument of most shape errors.
func (c context) argument() string {
	if c == findContext {
		return "projection"
	}
	return "$project (stage)"
}

func (c context) error(code handlererrors.ErrorCode, msg string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(code, c.prefix()+msg, c.argument())
}

const wrongPositionalLocationMessage = "As of 4.4, it's illegal to specify positional operator in the middle of a path." +
	"Positional projection may only be used at the end, " +
	"for example: a.b.$. If the query previously used a form " +
	"like a.b.$.d, remove the parts following the '$' and " +
	"the results will be equivalent."

const dollarPrefixMessage = "FieldPath field names may not start with '$'. Consider using $getField or $setField."

// ValidateProjection checks a $project specification and folds its constant
// subexpressions. It returns the validated specification, with numbers and
// booleans as booleans, and whether it is an inclusion projection.
func ValidateProjection(projection *types.Document) (*types.Document, bool, error) {
	validated, inclusion, err := validateShape(projection, stageContext)
	if err == nil {
		err = operators.CheckConstantSubexpressions(projection)
	}
	if err != nil {
		return nil, false, err
	}
	return validated, inclusion, nil
}

// ValidateProjectionShape is ValidateProjection without folding constant
// subexpressions, as an update pipeline parses $project.
func ValidateProjectionShape(projection *types.Document) (*types.Document, bool, error) {
	return validateShape(projection, stageContext)
}

// ValidateFindProjection is ValidateProjection for a find or findAndModify
// projection.
func ValidateFindProjection(projection *types.Document) (*types.Document, bool, error) {
	validated, inclusion, err := validateShape(projection, findContext)
	if err == nil {
		err = checkFindProjectionConstants(projection)
	}
	if err != nil {
		return nil, false, err
	}
	return validated, inclusion, nil
}

// checkFindProjectionConstants folds constant expressions in a find
// projection, skipping the $slice and $elemMatch projection operators, which
// are not expressions.
func checkFindProjectionConstants(value any) error {
	doc, ok := value.(*types.Document)
	if !ok {
		return operators.CheckConstantSubexpressions(value)
	}
	if operators.IsOperator(doc) {
		switch doc.Command() {
		case "$slice", "$elemMatch":
			return nil
		}
		return operators.CheckConstantSubexpressions(doc)
	}
	for _, v := range doc.Values() {
		if err := checkFindProjectionConstants(v); err != nil {
			return err
		}
	}
	return nil
}

func validateShape(projection *types.Document, ctx context) (*types.Document, bool, error) {
	if ctx == findContext && projection.Len() == 0 {
		return types.MakeDocument(0), false, nil
	}

	if err := CheckPathCollisions(projection, ctx.prefix(), ctx.argument()); err != nil {
		return nil, false, err
	}
	projection = FlattenProjection(projection)
	var deferred error

	if projection.Len() == 0 && ctx == stageContext {
		return nil, false, ctx.error(handlererrors.ErrEmptyProject, "projection specification must have at least one field")
	}

	validated := types.MakeDocument(0)
	var inclusion *bool

	iter := projection.Iterator()
	defer iter.Close()

	for {
		key, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, false, lazyerrors.Error(err)
		}

		positional := strings.HasSuffix(key, "$")
		if err = validatePath(key, positional, ctx); err != nil {
			return nil, false, err
		}

		var inclusionField bool
		computed := isComputedValue(value, ctx)
		if computed {
			if err = operators.CheckFieldPaths(value); err != nil {
				return nil, false, err
			}
		}

		switch value := value.(type) {
		case *types.Document:
			var neutral bool
			inclusionField, neutral, err = validateDocumentValue(key, value, inclusion, ctx)
			var cmdErr *handlererrors.CommandError
			if errors.As(err, &cmdErr) && cmdErr.Code() == handlererrors.ErrConversionFailure {
				// Evaluating a constant fails only after the shape checks.
				if deferred == nil {
					deferred = err
				}
				inclusionField, neutral, err = true, false, nil
			}
			if err != nil {
				return nil, false, err
			}
			validated.Set(key, value)
			if neutral {
				continue
			}

		case *types.Array, string, types.Binary, types.ObjectID,
			time.Time, types.NullType, types.Regex, types.Timestamp: // all these types are treated as new fields value
			inclusionField = true
			validated.Set(key, value)

		case float64, int32, int64, types.Decimal128:
			// projection treats 0 as false and any other value as true
			inclusionField = types.Compare(value, int32(0)) != types.Equal
			validated.Set(key, inclusionField)

		case bool:
			inclusionField = value
			validated.Set(key, inclusionField)

		default:
			return nil, false, lazyerrors.Errorf("unsupported operation %s %value (%T)", key, value, value)
		}

		if projection.Len() == 1 && key == "_id" {
			if deferred != nil {
				return nil, false, deferred
			}
			return validated, inclusionField, nil
		}

		if !inclusionField && positional {
			return nil, false, ctx.error(handlererrors.ErrExclusionPositionalProjection,
				"positional projection cannot be used with exclusion")
		}

		// A computed value makes the projection an inclusion; an _id flag
		// does not decide the mode.
		if computed && inclusion != nil && !*inclusion {
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrProjectionIDExpressionInExclusion,
				ctx.prefix()+fmt.Sprintf("Cannot use an expression %s: %s in an exclusion projection", key, types.FormatAnyValue(value)),
				"projection",
			)
		}

		// if inclusion is nil we are processing the first field
		if inclusion == nil {
			if key == "_id" && !computed {
				continue
			}
			inclusion = &inclusionField
			continue
		}

		if *inclusion != inclusionField {
			if key == "_id" {
				continue
			}
			if *inclusion {
				return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrProjectionExIn,
					ctx.prefix()+fmt.Sprintf("Cannot do exclusion on field %s in inclusion projection", key),
					"projection",
				)
			}
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrProjectionInEx,
				ctx.prefix()+fmt.Sprintf("Cannot do inclusion on field %s in exclusion projection", key),
				"projection",
			)
		}
	}

	if deferred != nil {
		return nil, false, deferred
	}

	if inclusion == nil {
		// Only find's $slice operators were specified (possibly alongside
		// _id). With _id included, only _id and the sliced arrays appear, as
		// in MongoDB; otherwise every field does, with the arrays sliced.
		if idVal, idErr := validated.Get("_id"); idErr == nil {
			if b, ok := idVal.(bool); ok && b {
				return validated, true, nil
			}
		}
		return validated, false, nil
	}

	return validated, *inclusion, nil
}

// isComputedValue reports whether a projection value is an expression
// rather than an inclusion or exclusion flag.
func isComputedValue(value any, ctx context) bool {
	switch value := value.(type) {
	case *types.Array, string, types.Binary, types.ObjectID,
		time.Time, types.NullType, types.Regex, types.Timestamp:
		return true
	case *types.Document:
		if ctx == findContext && value.Len() == 1 {
			switch value.Keys()[0] {
			case "$elemMatch", "$slice", "$meta":
				return false
			}
		}
		return operators.IsOperator(value)
	}
	return false
}

// validatePath reports mongod's errors for a projection key.
func validatePath(key string, positional bool, ctx context) error {
	if key == "" {
		return ctx.error(handlererrors.ErrEmptyFieldPath, "FieldPath cannot be constructed with empty string")
	}

	aggregatePositional := func() error {
		return ctx.error(handlererrors.ErrAggregatePositionalProject, "Cannot use positional projection in aggregation projection")
	}

	path, err := types.NewPathFromString(key)
	if err != nil {
		switch {
		case positional && ctx == findContext:
			return ctx.error(handlererrors.ErrInvalidFieldPath, "FieldPath must not end with a '.'.")
		case positional:
			return aggregatePositional()
		}
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrPathContainsEmptyElement,
			ctx.prefix()+"FieldPath field names may not be empty strings.", "projection")
	}

	if ctx == findContext {
		if path.Len() > 1 && strings.Count(path.TrimSuffix().String(), "$") > 1 {
			return ctx.error(handlererrors.ErrWrongPositionalOperatorLocation, wrongPositionalLocationMessage)
		}
		if strings.HasPrefix(key, "$") {
			return ctx.error(handlererrors.ErrFieldPathInvalidName, dollarPrefixMessage)
		}
		if path.Len() > 1 && slices.Contains(path.TrimSuffix().Slice(), "$") {
			return ctx.error(handlererrors.ErrWrongPositionalOperatorLocation, wrongPositionalLocationMessage)
		}
	} else {
		if key == "$" {
			return ctx.error(handlererrors.ErrFieldPathInvalidName, dollarPrefixMessage)
		}
		if positional {
			return aggregatePositional()
		}
		// With '$' at the prefix, the error is ErrFieldPathInvalidName below.
		if slices.Contains(strings.Split(strings.TrimPrefix(key, "$"), "."), "$") {
			return ctx.error(handlererrors.ErrWrongPositionalOperatorLocation, wrongPositionalLocationMessage)
		}
	}

	for _, k := range path.Slice() {
		// `v.$foo` is invalid; `v.$`, positional in find, and `v.foo$` are fine.
		if strings.HasPrefix(k, "$") && (k != "$" || ctx == stageContext) {
			return ctx.error(handlererrors.ErrFieldPathInvalidName, dollarPrefixMessage)
		}
	}
	return nil
}

// validateDocumentValue reports whether a document projection value
// includes, and whether it is neutral (find's $slice and $meta), which neither includes
// nor excludes. inclusion is the projection's mode so far, if known.
func validateDocumentValue(key string, value *types.Document, inclusion *bool, ctx context) (bool, bool, error) {
	if ctx == findContext && value.Len() == 1 {
		opVal := must.NotFail(value.Get(value.Keys()[0]))
		switch value.Keys()[0] {
		case "$elemMatch":
			if _, ok := opVal.(*types.Document); !ok {
				return false, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
					"$elemMatch projection requires an object specification", "$elemMatch")
			}
			return true, false, nil
		case "$slice":
			if err := validateSliceProjectionArg(opVal); err != nil {
				return false, false, err
			}
			return false, true, nil
		case "$meta":
			if _, err := operators.ParseMetaName(opVal); err != nil {
				return false, false, err
			}
			// $meta adds a field in either mode.
			return false, true, nil
		}
	}

	if ctx == stageContext && operators.IsOperator(value) {
		return true, false, ValidateExpression(value)
	}

	if value.Len() == 0 {
		return false, false, ctx.error(handlererrors.ErrEmptySubProject,
			"An empty sub-projection is not a valid value. Found empty object at path")
	}

	if ctx == findContext {
		if err := ValidateExpression(value); err != nil {
			return false, false, withoutStagePrefix(err)
		}
		if inclusion != nil && !*inclusion {
			return false, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrProjectionExpressionInExclusion,
				"Cannot use expression other than $meta in exclusion projection",
				"projection",
			)
		}
	}
	return true, false, nil
}

// validateSliceProjectionArg validates find's $slice projection argument: a
// number or a [skip, limit] array.
func validateSliceProjectionArg(arg any) error {
	switch v := arg.(type) {
	case int32, int64, float64:
		return nil
	case *types.Array:
		if v.Len() != 2 {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
				"$slice array argument should be of form [skip, limit]", "$slice")
		}
		for i := range 2 {
			switch must.NotFail(v.Get(i)).(type) {
			case int32, int64, float64:
			default:
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
					"$slice array argument should be of form [skip, limit]", "$slice")
			}
		}
		return nil
	default:
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
			fmt.Sprintf("First argument to $slice must be an array, but is of type %T", arg), "$slice")
	}
}

// withoutStagePrefix drops the "Invalid $project" context from an expression
// error, which MongoDB reports without it in find projections.
func withoutStagePrefix(err error) error {
	var ce *handlererrors.CommandError
	if !errors.As(err, &ce) {
		return err
	}
	msg, found := strings.CutPrefix(ce.Err().Error(), stageErrorPrefix)
	if !found {
		return err
	}
	return handlererrors.NewCommandErrorMsgWithArgument(ce.Code(), msg, "projection")
}
