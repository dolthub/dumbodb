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

// Package projection provides projection for aggregations.
package projection

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// ProjectDocument applies projection to the copy of the document.
func ProjectDocument(doc, projection *types.Document, inclusion bool) (*types.Document, error) {
	defer doc.ShowRecordKey()()
	if inclusion && operators.SpecHasComputedPath(projection, true) {
		projected, err := operators.ProjectInclusionTree(doc, projection)
		if err != nil {
			return nil, err
		}
		return projected, ProjectIDSubpaths(doc, projected, projection, inclusion)
	}

	projected, err := projectID(doc, projection)
	if err != nil {
		return nil, err
	}

	projectedWithoutID, err := projectDocumentWithoutID(doc, projection, inclusion)
	if err != nil {
		return nil, err
	}

	for _, key := range projectedWithoutID.Keys() {
		projected.Set(key, must.NotFail(projectedWithoutID.Get(key)))
	}

	if err = ProjectIDSubpaths(doc, projected, projection, inclusion); err != nil {
		return nil, err
	}

	return projected, nil
}

// ProjectIDSubpaths applies the flags on paths within _id, such as "_id.x",
// to the projected _id. An inclusion drops an _id that is not a document.
func ProjectIDSubpaths(doc, projected, projection *types.Document, inclusion bool) error {
	var paths []types.Path
	for _, key := range projection.Keys() {
		if _, isFlag := must.NotFail(projection.Get(key)).(bool); isFlag && strings.HasPrefix(key, "_id.") {
			path, err := types.NewPathFromString(strings.TrimPrefix(key, "_id."))
			if err != nil {
				return lazyerrors.Error(err)
			}
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	idValue, _ := doc.Get("_id")
	id, ok := idValue.(*types.Document)
	if !projected.Has("_id") {
		return nil
	}
	if !ok {
		if inclusion {
			projected.Remove("_id")
		}
		return nil
	}
	if !inclusion {
		trimmed := id.DeepCopy()
		for _, path := range paths {
			trimmed.RemoveByPath(path)
		}
		projected.Set("_id", trimmed)
		return nil
	}
	kept := types.MakeDocument(0)
	for _, path := range paths {
		if _, err := includeProjection(path, id, kept); err != nil {
			return err
		}
	}
	projected.Set("_id", kept)
	return nil
}

// topLevelInclusionFields returns the fields of a validated inclusion projection
// when it includes only top-level fields, which projectTopLevelInclusion applies.
func topLevelInclusionFields(projection *types.Document, inclusion bool) ([]string, bool) {
	if !inclusion {
		return nil, false
	}
	keys := projection.Keys()
	var fields []string
	for i, v := range projection.Values() {
		if keys[i] == "_id" {
			continue
		}
		include, ok := v.(bool)
		if !ok || !include || strings.ContainsAny(keys[i], ".$") {
			return nil, false
		}
		fields = append(fields, keys[i])
	}
	return fields, true
}

// projectTopLevelInclusion is ProjectDocument for a projection whose included
// fields are those returned by topLevelInclusionFields.
func projectTopLevelInclusion(doc, projection *types.Document, fields []string) (*types.Document, error) {
	projected, err := projectID(doc, projection)
	if err != nil {
		return nil, err
	}
	keys := doc.Keys()
	for i, v := range doc.Values() {
		if keys[i] != "_id" && slices.Contains(fields, keys[i]) {
			projected.Set(keys[i], v)
		}
	}
	return projected, nil
}

// projectID starts a projected document with the _id the projection keeps or
// sets.
func projectID(doc, projection *types.Document) (*types.Document, error) {
	// _id carries through by default, but only when the input document
	// actually has it. Pipeline stages can emit documents without _id
	// (e.g. an earlier {$project: {_id: 0}}, {$group}, {$unwind} of a
	// projected stream), and a later inclusion $project over those must
	// not panic -- MongoDB simply yields no _id in that case. An explicit
	// _id in the projection is still honored below regardless.
	projected := must.NotFail(types.NewDocument())
	if idValue, idErr := doc.Get("_id"); idErr == nil {
		projected.Set("_id", idValue)
	}

	var err error

	if projection.Has("_id") {
		idValue := must.NotFail(projection.Get("_id"))

		var set bool

		switch idValue := idValue.(type) {
		case *types.Document: // field: { $elemMatch: { field2: value }}
			var op operators.Operator
			var value any

			if !operators.IsOperator(idValue) {
				projected.Set("_id", idValue)
				set = true

				break
			}

			op, err = operators.NewOperator(idValue)
			if err != nil {
				return nil, processOperatorError(err)
			}

			value, err = op.Process(doc)
			if err != nil {
				if errors.Is(err, operators.ErrMissingValue) {
					break
				}

				return nil, err
			}

			set = true
			projected.Set("_id", value)

		case *types.Array:
			arr, err := operators.EvalArrayExpression(idValue, doc)
			if err != nil {
				return nil, err
			}
			projected.Set("_id", arr)

			set = true

		case string:
			if !strings.HasPrefix(idValue, "$") {
				projected.Set("_id", idValue)
				set = true

				break
			}

			var expr *aggregations.Expression
			if expr, err = aggregations.NewExpression(idValue, nil); err != nil {
				return nil, processOperatorError(err)
			}

			var value any
			if value, err = expr.Evaluate(doc); err != nil {
				break
			}

			projected.Set("_id", value)

			set = true

		case types.Binary, types.ObjectID,
			time.Time, types.NullType, types.Regex, types.Timestamp: // all this types are treated as new fields value
			projected.Set("_id", idValue)

			set = true

		case bool:
			set = idValue

		default:
			return nil, lazyerrors.Errorf("unsupported operation %s %v (%T)", "_id", idValue, idValue)
		}

		if !set {
			projected.Remove("_id")
		}
	}

	return projected, nil
}

// projectDocumentWithoutID applies projection to the document and returns the projected document.
// It ignores the _id field in the projection (the caller handles _id).
//
// The function does not mutate doc or projection: inclusion builds a fresh
// output document; exclusion starts from a single deep copy of doc (minus
// _id) and removes excluded fields in place. A previous implementation
// deep-copied doc, projection, and (on exclusion) the already-copied doc
// again on every input document  -- a hot-path cost that dominated $project.
func projectDocumentWithoutID(doc *types.Document, projection *types.Document, inclusion bool) (*types.Document, error) {
	var projected *types.Document
	if inclusion {
		projected = types.MakeDocument(0)
	} else {
		projected = doc.DeepCopy()
		projected.Remove("_id")
	}

	iter := projection.Iterator()
	defer iter.Close()

	for {
		key, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		// _id is handled separately by ProjectDocument; skip it here so we
		// don't need to allocate a copy of the projection just to strip _id.
		if key == "_id" {
			continue
		}

		path, err := types.NewPathFromString(key)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		switch value := value.(type) { // found in the projection
		case *types.Document: // field: { $elemMatch: { field2: value }}
			var op operators.Operator
			var v any

			if !operators.IsOperator(value) {
				projected.Set(key, value)
				break
			}

			op, err = operators.NewOperator(value)
			if err != nil {
				return nil, processOperatorError(err)
			}

			v, err = op.Process(doc)
			if err != nil {
				if errors.Is(err, operators.ErrMissingValue) {
					continue
				}

				return nil, err
			}

			projected.Set(key, v)

		case string:
			if strings.HasPrefix(value, "$") {
				expr, err := aggregations.NewExpression(value, nil)
				if err != nil {
					return nil, processOperatorError(err)
				}

				v, err := expr.Evaluate(doc)
				if err != nil {
					// Field not found: omit from result (MongoDB behavior for missing field references).
					continue
				}

				projected.Set(key, v)
			} else {
				projected.Set(key, value)
			}

		case *types.Array:
			arr, err := operators.EvalArrayExpression(value, doc)
			if err != nil {
				return nil, err
			}
			projected.Set(key, arr)

		case types.Binary, types.ObjectID,
			time.Time, types.NullType, types.Regex, types.Timestamp: // all these types are treated as new fields value
			projected.Set(key, value)

		case bool: // field: bool
			if inclusion {
				// includeProjection reads fields from doc and writes to
				// projected. It never mutates source, so we pass doc
				// directly. _id never appears in path (handled above) so
				// the _id field of doc is harmless here.
				if _, err = includeProjection(path, doc, projected); err != nil {
					return nil, err
				}

				continue
			}

			// exclusion projection removes the field on the path in projected.
			excludeProjection(path, projected)
		default:
			return nil, lazyerrors.Errorf("unsupported operation %s %v (%T)", key, value, value)
		}
	}

	return projected, nil
}

// includeProjection copies the field on the path from source to projected.
// When an array is on the path, it returns the array containing any document
// with the same key. Dot notation with array index path does not include
// the field unlike document.SetByPath(path).
// Inclusion projection with non-existent path creates an empty document
// or an empty array based on what source has.
// It returns iterator errors other than ErrIteratorDone.
// If the projected contains field that is not expected in source, it panics.
//
//	Example: "v.foo" path inclusion projection:
//	{v: {foo: 1, bar: 1}}               -> {v: {foo: 1}}
//	{v: {bar: 1}}                       -> {v: {}}
//	{v: [{bar: 1}]}                     -> {v: [{}]}
//	{v: [{foo: 1}, {foo: 2}, {bar: 1}]} -> {v: [{foo: 1}, {foo: 2}, {}]}
//
//	Example: "v.0.foo" path inclusion projection:
//	{v: [{foo: 1}, {foo: 2}, {bar: 1}]} -> {v: [{}, {}, {}]}
func includeProjection(path types.Path, source any, projected *types.Document) (*types.Array, error) {
	key := path.Prefix()

	switch source := source.(type) {
	case *types.Document:
		embeddedSource, err := source.Get(key)
		if err != nil {
			// key does not exist, nothing to set.
			return nil, nil
		}

		if path.Len() <= 1 {
			// path reached suffix, set field in projected.
			setBySourceOrder(key, embeddedSource, source, projected)
			return nil, nil
		}

		doc := new(types.Document)

		if projected.Has(key) {
			// set doc if projected has field from other projection field.
			v := must.NotFail(projected.Get(key))
			if d, ok := v.(*types.Document); ok {
				doc = d
			}

			if arr, ok := v.(*types.Array); ok {
				// use next prefix key with arr value, allowing array to parse existing
				// projection fields.
				doc = must.NotFail(types.NewDocument(path.TrimPrefix().Prefix(), arr))
			}
		}

		// when next prefix has an array use returned value arr,
		// if it has a document, field in the doc is set by includeProjection.
		arr, err := includeProjection(path.TrimPrefix(), embeddedSource, doc)
		if err != nil {
			return nil, err
		}

		switch embeddedSource.(type) {
		case *types.Document:
			setBySourceOrder(key, doc, source, projected)
		case *types.Array:
			projected.Set(key, arr)
		}

		return nil, nil
	case *types.Array:
		iter := source.Iterator()
		defer iter.Close()

		arr := new(types.Array)
		var inclusionExists bool

		if v, err := projected.Get(key); err == nil {
			projectedArr, ok := v.(*types.Array)
			if ok {
				arr = projectedArr
				inclusionExists = true
			}
		}

		i := 0

		for {
			_, arrElem, err := iter.Next()
			if err != nil {
				if errors.Is(err, iterator.ErrIteratorDone) {
					break
				}

				return nil, lazyerrors.Error(err)
			}

			if _, ok := arrElem.(*types.Document); !ok {
				continue
			}

			doc := new(types.Document)

			if inclusionExists {
				// when there are multiple inclusion fields, first inclusion
				// inserts all documents from source to arr, they could be empty
				// if it did not match previous inclusion fields.
				// But number of documents in arr must be the same as number of documents
				// in source.
				var v any

				v, err = arr.Get(i)
				if err != nil {
					panic(err)
				}

				docVal, ok := v.(*types.Document)
				if !ok {
					panic("projected field must be a document")
				}

				doc = docVal
			} else {
				// first inclusion field, insert it to the doc.
				arr.Append(doc)
			}

			if _, err = includeProjection(path, arrElem, doc); err != nil {
				return nil, err
			}

			arr.Set(i, doc)
			i++
		}

		return arr, nil
	default:
		// field is not a document or an array, nothing to set.
		return nil, nil
	}
}

// excludeProjection removes the field on the path in projected.
// When an array is on the path, it checks if the array contains any document
// with the key to remove that document. This is not the case in document.Remove(key).
// Dot notation with array index path do not exclude unlike document.RemoveByPath(key).
//
//	Examples: "v.foo" path exclusion projection:
//	{v: {foo: 1}}                       -> {v: {}}
//	{v: {foo: 1, bar: 1}}               -> {v: {bar: 1}}
//	{v: [{foo: 1}, {foo: 2}]}           -> {v: [{}, {}]}
//	{v: [{foo: 1}, {foo: 2}, {bar: 1}]} -> {v: [{}, {}, {bar: 1}]}
//
//	Example: "v.0.foo" path exclusion projection:
//	{v: [{foo: 1}, {foo: 2}]}           -> {v: [{foo: 1}, {foo: 2}]}
func excludeProjection(path types.Path, projected any) {
	key := path.Prefix()

	switch projected := projected.(type) {
	case *types.Document:
		embeddedSource, err := projected.Get(key)
		if err != nil {
			// key does not exist, nothing to exclude.
			return
		}

		if path.Len() <= 1 {
			// path reached suffix, remove the field from the document.
			projected.Remove(key)
			return
		}

		// recursively remove field from the embeddedSource.
		excludeProjection(path.TrimPrefix(), embeddedSource)

		return
	case *types.Array:
		// modifies the field of projected, hence not using iterator.
		for i := 0; i < projected.Len(); i++ {
			arrElem := must.NotFail(projected.Get(i))

			if _, ok := arrElem.(*types.Document); !ok {
				// not a document, cannot possibly be part of path, do nothing.
				continue
			}

			excludeProjection(path, arrElem)
		}

		return
	default:
		// not a path, nothing to exclude.
		return
	}
}

// setBySourceOrder sets the key value field to projected in same field order as the source.
// Example:
//
//	key: foo
//	val: 1
//	source: {foo: 1, bar: 2}
//	projected: {bar: 2}
//
// setBySourceOrder sets projected to {foo: 1, bar: 2} rather than adding it to the last field.
func setBySourceOrder(key string, val any, source, projected *types.Document) {
	projectedKeys := projected.Keys()

	// newFieldIndex is where new field is to be inserted in projected document.
	newFieldIndex := 0

	for _, sourceKey := range source.Keys() {
		if sourceKey == key {
			break
		}

		if newFieldIndex >= len(projectedKeys) {
			break
		}

		if sourceKey == projectedKeys[newFieldIndex] {
			newFieldIndex++
		}
	}

	tmp := projected.DeepCopy()

	// remove fields of projected from newFieldIndex to the end
	for i := newFieldIndex; i < len(projectedKeys); i++ {
		projected.Remove(projectedKeys[i])
	}

	projected.Set(key, val)

	// copy newFieldIndex-th to the end from tmp to projected
	i := newFieldIndex
	for _, key := range tmp.Keys()[newFieldIndex:] {
		projected.Set(key, must.NotFail(tmp.Get(tmp.Keys()[i])))
		i++
	}
}

// ValidateExpression reports the parse errors mongod reports for an
// expression object used as a projection value.
func ValidateExpression(value *types.Document) error {
	op, err := operators.NewOperator(value)
	if err = processOperatorError(err); err != nil {
		return err
	}

	// Only shape errors matter against the placeholder document; data
	// errors surface when the stage runs, as in MongoDB.
	if _, err = op.Process(must.NotFail(types.NewDocument("key", "value"))); err != nil {
		var opErr operators.OperatorError
		var exErr *aggregations.ExpressionError

		if errors.As(err, &opErr) || errors.As(err, &exErr) {
			return processOperatorError(operators.AsNestedError(err))
		}
	}

	return nil
}

// processOperatorError takes internal error related to operator evaluation and
// returns proper CommandError that can be returned by $project aggregation stage.
//
// Command error codes:
// - ErrEmptySubProject when operator value is empty.
// - ErrFieldPathInvalidName when FieldPath is invalid.
// - ErrNotImplemented when the operator or expression is not implemented yet.
// - ErrOperatorWrongLenOfArgs when the operator has an invalid number of arguments.
// - ErrInvalidPipelineOperator when the operator does not exist.
// - ErrFailedToParse when operator has invalid variable expression.
// - ErrGroupInvalidFieldPath when operator has empty path expression.
func processOperatorError(err error) error {
	if err == nil {
		return nil
	}

	var opErr operators.OperatorError
	var exErr *aggregations.ExpressionError

	switch {
	case errors.As(err, &opErr):
		switch opErr.Code() {
		case operators.ErrTooManyFields:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFieldPathInvalidName,
				"Invalid $project :: caused by :: FieldPath field names may not start with '$'."+
					" Consider using $getField or $setField.",
				"$project (stage)",
			)
		case operators.ErrNotImplemented:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNotImplemented,
				"Invalid $project :: caused by :: "+opErr.Error(),
				"$project (stage)",
			)
		case operators.ErrArgsInvalidLen:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrOperatorWrongLenOfArgs,
				"Invalid $project :: caused by :: "+opErr.Error(),
				"$project (stage)",
			)
		case operators.ErrInvalidExpression:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrAggregateInvalidExpression,
				fmt.Sprintf("Invalid $project :: caused by :: Unknown expression %s", opErr.Name()),
				"$project (stage)",
			)
		case operators.ErrInvalidNestedExpression:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidPipelineOperator,
				"Invalid $project :: caused by :: "+opErr.Error(),
				"$project (stage)",
			)
		}

		// The field operators each carry their own MongoDB error code, so they
		// are mapped by identity rather than enumerated here.
		if code, ok := operators.FieldOpErrorCode(err); ok {
			return handlererrors.NewCommandErrorMsgWithArgument(
				code,
				"Invalid $project :: caused by :: "+opErr.Error(),
				"$project (stage)",
			)
		}

	case errors.As(err, &exErr):
		switch exErr.Code() {
		case aggregations.ErrNotExpression:
			// handled by upstream and this should not be reachable for existing expression implementation
			fallthrough
		case aggregations.ErrInvalidExpression:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				"Invalid $project :: caused by :: '$' starts with an invalid character for a user variable name",
				"$project (stage)",
			)
		case aggregations.ErrEmptyFieldPath:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrGroupInvalidFieldPath,
				"Invalid $project :: caused by :: '$' by itself is not a valid FieldPath",
				"$project (stage)",
			)
		case aggregations.ErrUndefinedVariable:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNotImplemented,
				"Aggregation expression variables are not implemented yet",
				"$project (stage)",
			)
		case aggregations.ErrEmptyVariable:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				"Invalid $project :: caused by :: empty variable names are not allowed",
				"$project (stage)",
			)
		}
	}

	return lazyerrors.Error(err)
}

// CheckPathCollisions returns MongoDB's error for a projection with one path
// inside another or repeated, such as "a" and "a.b", counting the paths of
// nested specification objects; msgPrefix and argument shape it for the
// command.
func CheckPathCollisions(projection *types.Document, msgPrefix, argument string) error {
	var visited []types.Path
	for _, key := range specPaths(projection, "") {
		path, err := types.NewPathFromString(key)
		if err != nil {
			continue
		}
		var pathErr *types.PathError
		if errors.As(types.IsConflictPath(visited, path), &pathErr) {
			switch pathErr.Code() {
			case types.ErrPathConflictOverwrite:
				return handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrUnsetPathOverwrite,
					fmt.Sprintf("%sPath collision at %s", msgPrefix, key),
					argument,
				)
			case types.ErrPathConflictCollision:
				return handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrUnsetPathCollision,
					fmt.Sprintf("%sPath collision at %s remaining portion %s", msgPrefix, key, pathErr.Error()),
					argument,
				)
			}
		}
		visited = append(visited, path)
	}
	return nil
}

// specPaths returns the dotted path of every field projection specifies,
// expanding nested specification objects.
func specPaths(projection *types.Document, prefix string) []string {
	var paths []string
	for _, key := range projection.Keys() {
		value := must.NotFail(projection.Get(key))
		if nested, ok := value.(*types.Document); ok && nested.Len() > 0 && !strings.HasPrefix(nested.Keys()[0], "$") {
			paths = append(paths, specPaths(nested, prefix+key+".")...)
			continue
		}
		paths = append(paths, prefix+key)
	}
	return paths
}

// FlattenProjection rewrites nested specification objects as dotted keys, so
// {a: {b: 1, c: "$x"}} reads as {"a.b": 1, "a.c": "$x"}. Operator objects and
// empty objects are kept as values.
func FlattenProjection(projection *types.Document) *types.Document {
	out := types.MakeDocument(projection.Len())
	for _, key := range projection.Keys() {
		value := must.NotFail(projection.Get(key))
		nested, ok := value.(*types.Document)
		if !ok || nested.Len() == 0 || strings.HasPrefix(nested.Keys()[0], "$") {
			out.Set(key, value)
			continue
		}
		flat := FlattenProjection(nested)
		for _, sub := range flat.Keys() {
			out.Set(key+"."+sub, must.NotFail(flat.Get(sub)))
		}
	}
	return out
}
