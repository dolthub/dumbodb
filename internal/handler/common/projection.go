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

package common

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// ValidateProjection check projection document.
// Document fields could be either included or excluded but not both.
// Exception is for the _id field that could be included or excluded.
// ValidateProjection returns errProjectionEmpty for empty projection and
// CommandError for invalid projection fields.
//
// Command error codes:
//   - `ErrProjectionExIn` when there is exclusion in inclusion projection;
//   - `ErrProjectionInEx` when there is inclusion in exclusion projection;
//   - `ErrEmptyFieldPath` when projection path is empty;
//   - `ErrInvalidFieldPath` when positional projection path contains empty key;
//   - `ErrPathContainsEmptyElement` when projection path contains empty key;
//   - `ErrFieldPathInvalidName` when `$` is at the prefix of a key in the path;
//   - `ErrWrongPositionalOperatorLocation` when there are multiple `$`;
//   - `ErrExclusionPositionalProjection` when positional projection is used for exclusion;
//   - `ErrBadPositionalProjection` when array or filter at positional projection path is empty;
//   - `ErrBadPositionalProjection` when there is no filter field key for positional projection path;
//   - `ErrElementMismatchPositionalProjection` when unexpected array was found on positional projection path;
//   - `ErrNotImplemented` when there is unimplemented projection operators and expressions.
func ValidateProjection(projection *types.Document) (*types.Document, bool, error) {
	validated := types.MakeDocument(0)

	if projection.Len() == 0 {
		// empty projection is exclusion project.
		return types.MakeDocument(0), false, nil
	}

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

		if key == "" {
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrEmptyFieldPath,
				"FieldPath cannot be constructed with empty string",
				"projection",
			)
		}

		positionalProjection := strings.HasSuffix(key, "$")

		path, err := types.NewPathFromString(key)
		if err != nil {
			if positionalProjection {
				return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrInvalidFieldPath,
					"FieldPath must not end with a '.'.",
					"projection",
				)
			}

			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrPathContainsEmptyElement,
				"FieldPath field names may not be empty strings.",
				"projection",
			)
		}

		if path.Len() > 1 && strings.Count(path.TrimSuffix().String(), "$") > 1 {
			// there cannot be more than one positional operator.
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrWrongPositionalOperatorLocation,
				"As of 4.4, it's illegal to specify positional operator in the middle of a path."+
					"Positional projection may only be used at the end, "+
					"for example: a.b.$. If the query previously used a form "+
					"like a.b.$.d, remove the parts following the '$' and "+
					"the results will be equivalent.",
				"projection",
			)
		}

		if key == "$" || strings.HasPrefix(key, "$") {
			// positional operator cannot be at the prefix.
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFieldPathInvalidName,
				"FieldPath field names may not start with '$'. Consider using $getField or $setField.",
				"projection",
			)
		}

		if path.Len() > 1 && slices.Contains(path.TrimSuffix().Slice(), "$") {
			// there cannot be a positional operator along the path, can only be at the end.
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrWrongPositionalOperatorLocation,
				"As of 4.4, it's illegal to specify positional operator in the middle of a path."+
					"Positional projection may only be used at the end, "+
					"for example: a.b.$. If the query previously used a form "+
					"like a.b.$.d, remove the parts following the '$' and "+
					"the results will be equivalent.",
				"projection",
			)
		}

		for _, k := range path.Slice() {
			if strings.HasPrefix(k, "$") && k != "$" {
				// arbitrary `$` cannot exist in the path,
				// `v.$foo` is invalid, `v.$` and `v.foo$` are fine.
				return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrFieldPathInvalidName,
					"FieldPath field names may not start with '$'. Consider using $getField or $setField.",
					"projection",
				)
			}
		}

		var inclusionField bool

		switch value := value.(type) {
		case *types.Document:
			if value.Len() != 1 {
				return nil, false, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrNotImplemented,
					fmt.Sprintf("projection expression %s is not supported", types.FormatAnyValue(value)),
				)
			}

			opKey := value.Keys()[0]
			opVal := must.NotFail(value.Get(opKey))

			switch opKey {
			case "$elemMatch":
				if _, ok := opVal.(*types.Document); !ok {
					return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						"$elemMatch projection requires an object specification",
						"$elemMatch",
					)
				}

				inclusionField = true
				validated.Set(key, value)

			case "$slice":
				if err := validateSliceProjectionArg(opVal); err != nil {
					return nil, false, err
				}

				validated.Set(key, value)
				// $slice is neutral - does not affect inclusion/exclusion mode tracking.
				continue

			case "$meta":
				metaStr, ok := opVal.(string)
				if !ok {
					return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						"$meta projection only supports \"textScore\" and \"indexKey\" right now",
						"$meta",
					)
				}

				switch metaStr {
				case "textScore", "indexKey":
				default:
					return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						"$meta projection only supports \"textScore\" and \"indexKey\" right now",
						"$meta",
					)
				}

				inclusionField = true
				validated.Set(key, value)

			default:
				return nil, false, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrNotImplemented,
					fmt.Sprintf("projection expression %s is not supported", types.FormatAnyValue(value)),
				)
			}

		case *types.Array, string, types.Binary, types.ObjectID,
			time.Time, types.NullType, types.Regex, types.Timestamp: // all these types are treated as new fields value
			inclusionField = true

			validated.Set(key, value)
		case float64, int32, int64:
			// projection treats 0 as false and any other value as true
			comparison := types.Compare(value, int32(0))

			if comparison != types.Equal {
				inclusionField = true
			}

			// set the value with boolean inclusionField to omit type assertion when we will apply projection
			validated.Set(key, inclusionField)
		case bool:
			inclusionField = value

			// set the value with boolean inclusionField to omit type assertion when we will apply projection
			validated.Set(key, inclusionField)
		default:
			return nil, false, lazyerrors.Errorf("unsupported operation %s %value (%T)", key, value, value)
		}

		if projection.Len() == 1 && key == "_id" {
			return validated, inclusionField, nil
		}

		if !inclusionField && positionalProjection {
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrExclusionPositionalProjection,
				"positional projection cannot be used with exclusion",
				"projection",
			)
		}

		// if inclusion is nil we are processing the first field
		if inclusion == nil {
			if key == "_id" {
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
					fmt.Sprintf("Cannot do exclusion on field %s in inclusion projection", key),
					"projection",
				)
			}

			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrProjectionInEx,
				fmt.Sprintf("Cannot do inclusion on field %s in exclusion projection", key),
				"projection",
			)
		}
	}

	if inclusion == nil {
		// Only $slice operators were specified (possibly alongside _id).
		// If _id was explicitly included (value true), use inclusion mode so
		// only _id and the sliced arrays appear  -- matching MongoDB behaviour for
		// projections like {_id: 1, scores: {$slice: 2}}.
		if idVal, idErr := validated.Get("_id"); idErr == nil {
			if b, ok := idVal.(bool); ok && b {
				return validated, true, nil
			}
		}
		// Otherwise default to exclusion mode (show everything with array
		// modifications applied).
		return validated, false, nil
	}

	return validated, *inclusion, nil
}

// ProjectDocument applies projection to the copy of the document.
// It returns proper CommandError that can be returned by $project aggregation stage.
//
// Command error codes:
// - ErrEmptySubProject when operator value is empty.
// - ErrFieldPathInvalidName when FieldPath is invalid.
// - ErrNotImplemented when the operator is not implemented yet.
// - ErrOperatorWrongLenOfArgs when the operator has an invalid number of arguments.
// - ErrInvalidPipelineOperator when an the operator does not exist.
func ProjectDocument(doc, projection, filter *types.Document, inclusion bool) (*types.Document, error) {
	projected, err := types.NewDocument("_id", must.NotFail(doc.Get("_id")))
	if err != nil {
		return nil, err
	}

	projected.SetRecordID(doc.RecordID())

	if projection.Has("_id") {
		idValue := must.NotFail(projection.Get("_id"))

		var set bool

		switch idValue := idValue.(type) {
		case *types.Document: // field: { $elemMatch: { field2: value }}
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrCommandNotFound,
				fmt.Sprintf("projection %s is not supported",
					types.FormatAnyValue(idValue),
				),
			)

		case *types.Array, string, types.Binary, types.ObjectID,
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

	projectedWithoutID, err := projectDocumentWithoutID(doc, projection, filter, inclusion)
	if err != nil {
		return nil, err
	}

	for _, key := range projectedWithoutID.Keys() {
		projected.Set(key, must.NotFail(projectedWithoutID.Get(key)))
	}

	return projected, nil
}

// projectDocumentWithoutID applies projection to the copy of the document and returns projected document.
// It ignores _id field in the projection.
func projectDocumentWithoutID(doc *types.Document, projection, filter *types.Document, inclusion bool) (*types.Document, error) {
	projectionWithoutID := projection.DeepCopy()
	projectionWithoutID.Remove("_id")

	docWithoutID := doc.DeepCopy()
	docWithoutID.Remove("_id")

	projected := types.MakeDocument(0)

	if !inclusion {
		projected = docWithoutID.DeepCopy()
	}

	iter := projectionWithoutID.Iterator()
	defer iter.Close()

	for {
		key, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		path, err := types.NewPathFromString(key)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		switch value := value.(type) { // found in the projection
		case *types.Document: // field: { $elemMatch: {...} } | { $slice: N } | { $meta: "..." }
			opKey := value.Keys()[0]
			opVal := must.NotFail(value.Get(opKey))

			switch opKey {
			case "$slice":
				if err = applySliceProjection(key, path, opVal, docWithoutID, projected, inclusion); err != nil {
					return nil, err
				}

			case "$elemMatch":
				condition := opVal.(*types.Document) //nolint:forcetypeassert // validated in ValidateProjection
				if err = applyElemMatchProjection(key, path, condition, docWithoutID, projected); err != nil {
					return nil, err
				}

			case "$meta":
				metaType := opVal.(string) //nolint:forcetypeassert // validated in ValidateProjection
				applyMetaProjection(key, metaType, projected)

			default:
				return nil, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrCommandNotFound,
					fmt.Sprintf("projection %s is not supported", types.FormatAnyValue(value)),
				)
			}

		case *types.Array, string, types.Binary, types.ObjectID,
			time.Time, types.NullType, types.Regex, types.Timestamp: // all these types are treated as new fields value
			projected.Set(key, value)

		case bool: // field: bool
			if inclusion {
				// inclusion projection copies the field on the path from docWithoutID to projected.
				if _, err = includeProjection(path, 0, docWithoutID, projected, filter); err != nil {
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

// validateSliceProjectionArg validates the $slice projection argument.
// Valid forms: integer (int32/int64/float64) or [skip, limit] array.
func validateSliceProjectionArg(arg any) error {
	switch v := arg.(type) {
	case int32, int64, float64:
		return nil
	case *types.Array:
		if v.Len() != 2 {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$slice array argument should be of form [skip, limit]",
				"$slice",
			)
		}

		for i := range 2 {
			elem, _ := v.Get(i)
			switch elem.(type) {
			case int32, int64, float64:
			default:
				return handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$slice array argument should be of form [skip, limit]",
					"$slice",
				)
			}
		}

		return nil
	default:
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("First argument to $slice must be an array, but is of type %T", arg),
			"$slice",
		)
	}
}

// sliceArray returns the first n elements (n > 0) or last |n| elements (n < 0) of arr.
// If |n| >= arr.Len(), the full array is returned.
func sliceArray(arr *types.Array, n int) *types.Array {
	result := new(types.Array)
	length := arr.Len()

	if n >= 0 {
		end := n
		if end > length {
			end = length
		}

		for i := range end {
			result.Append(must.NotFail(arr.Get(i)))
		}
	} else {
		// negative: take last |n| elements
		start := length + n
		if start < 0 {
			start = 0
		}

		for i := start; i < length; i++ {
			result.Append(must.NotFail(arr.Get(i)))
		}
	}

	return result
}

// sliceArrayWithSkip returns at most limit elements starting from skip.
// Negative skip counts from the end.
func sliceArrayWithSkip(arr *types.Array, skip, limit int) *types.Array {
	length := arr.Len()

	start := skip
	if start < 0 {
		start = length + skip
		if start < 0 {
			start = 0
		}
	} else if start > length {
		start = length
	}

	end := start + limit
	if limit <= 0 {
		end = start
	}

	if end > length {
		end = length
	}

	result := new(types.Array)
	for i := start; i < end; i++ {
		result.Append(must.NotFail(arr.Get(i)))
	}

	return result
}

// applySliceArg applies a $slice argument to an array and returns the sliced array.
func applySliceArg(arr *types.Array, arg any) (*types.Array, error) {
	switch v := arg.(type) {
	case int32:
		return sliceArray(arr, int(v)), nil
	case int64:
		return sliceArray(arr, int(v)), nil
	case float64:
		return sliceArray(arr, int(v)), nil
	case *types.Array:
		// [skip, limit]
		skipVal, _ := v.Get(0)
		limitVal, _ := v.Get(1)

		var skip, limit int

		switch s := skipVal.(type) {
		case int32:
			skip = int(s)
		case int64:
			skip = int(s)
		case float64:
			skip = int(s)
		}

		switch l := limitVal.(type) {
		case int32:
			limit = int(l)
		case int64:
			limit = int(l)
		case float64:
			limit = int(l)
		}

		return sliceArrayWithSkip(arr, skip, limit), nil
	default:
		return nil, lazyerrors.Errorf("unexpected $slice argument type %T", arg)
	}
}

// applySliceProjection applies a $slice projection operator to the field at path.
// In inclusion mode, the (sliced) field is fetched from source and placed in projected.
// In exclusion mode, the field already exists in projected and is replaced with its sliced version.
func applySliceProjection(key string, path types.Path, sliceArg any, source, projected *types.Document, inclusion bool) error {
	var fieldVal any

	if inclusion {
		// Get the value from source to include it.
		v, err := source.GetByPath(path)
		if err != nil {
			// Field doesn't exist in source; nothing to include.
			return nil
		}

		fieldVal = v
	} else {
		// The field is already in projected; get it and replace.
		v, err := projected.GetByPath(path)
		if err != nil {
			// Field doesn't exist in projected; nothing to modify.
			return nil
		}

		fieldVal = v
	}

	arr, ok := fieldVal.(*types.Array)
	if !ok {
		// Not an array; include as-is in inclusion mode, leave unchanged in exclusion mode.
		if inclusion {
			setBySourceOrder(key, fieldVal, source, projected)
		}

		return nil
	}

	sliced, err := applySliceArg(arr, sliceArg)
	if err != nil {
		return err
	}

	if path.Len() == 1 {
		if inclusion {
			setBySourceOrder(key, sliced, source, projected)
		} else {
			projected.Set(key, sliced)
		}
	} else {
		// Nested path: use SetByPath.
		if err = projected.SetByPath(path, sliced); err != nil {
			return lazyerrors.Error(err)
		}
	}

	return nil
}

// applyElemMatchProjection applies a $elemMatch projection.
// It finds the first element in the array at path that matches condition,
// and places it as a single-element array in projected.
// If the field is not an array or no element matches, the field is excluded.
func applyElemMatchProjection(key string, path types.Path, condition *types.Document, source, projected *types.Document) error {
	fieldVal, err := source.GetByPath(path)
	if err != nil {
		// Field doesn't exist; exclude.
		return nil
	}

	arr, ok := fieldVal.(*types.Array)
	if !ok {
		// Not an array; exclude.
		return nil
	}

	elem, found, err := findFirstElemMatchElement(arr, condition)
	if err != nil {
		return err
	}

	if found {
		result := new(types.Array)
		result.Append(elem)

		if path.Len() == 1 {
			setBySourceOrder(key, result, source, projected)
		} else {
			if err = projected.SetByPath(path, result); err != nil {
				return lazyerrors.Error(err)
			}
		}
	}

	// If not found, field is excluded (not added to projected).
	return nil
}

// findFirstElemMatchElement returns the first element in arr that matches condition.
// For operator conditions (keys starting with $), it wraps the element in a document.
// For field conditions (keys not starting with $), it treats element as a document.
func findFirstElemMatchElement(arr *types.Array, condition *types.Document) (any, bool, error) {
	iter := arr.Iterator()
	defer iter.Close()

	// Determine if condition uses operators or field names.
	isOperatorCondition := true

	for _, k := range condition.Keys() {
		if !strings.HasPrefix(k, "$") {
			isOperatorCondition = false
			break
		}
	}

	for {
		_, elem, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, false, lazyerrors.Error(err)
		}

		var matches bool

		if isOperatorCondition {
			// Wrap scalar/array element in a document with a dummy key to apply operator filter.
			wrapDoc := must.NotFail(types.NewDocument("__v__", elem))
			matchFilter := must.NotFail(types.NewDocument("__v__", condition))
			matches, err = FilterDocument(wrapDoc, matchFilter)
		} else {
			// Element must be a document to match field conditions.
			if elemDoc, ok := elem.(*types.Document); ok {
				matches, err = FilterDocument(elemDoc, condition)
			}
		}

		if err != nil {
			return nil, false, lazyerrors.Error(err)
		}

		if matches {
			return elem, true, nil
		}
	}

	return nil, false, nil
}

// applyMetaProjection sets a $meta projection value in projected.
// textScore returns 0.0 (DumboDB does not support text search).
// indexKey returns an empty document.
func applyMetaProjection(key, metaType string, projected *types.Document) {
	switch metaType {
	case "textScore":
		projected.Set(key, float64(0))
	case "indexKey":
		projected.Set(key, types.MakeDocument(0))
	}
}

// includeProjection copies the field on the path from source to projected.
// When an array is on the path, it returns the array containing any document
// with the same key. Dot notation with array index path does not include
// the field unlike document.SetByPath(path).
// Inclusion projection with non-existent path creates an empty document
// or an empty array based on what source has.
// Positional projection returns the first element of an array which matches the filter condition.
// If the projected contains field that is not expected in source, it panics.
//
// Command error codes:
//   - ErrBadPositionalProjection when array or filter at positional projection path is empty.
//   - ErrBadPositionalProjection when there is no filter field key for positional projection path.
//     If positional projection is `v.$`, the filter must contain `v` in the filter key such as `{v: 42}`.
//   - ErrElementMismatchPositionalProjection when unexpected array was found on positional projection path.
//
// Example: "v.foo" path inclusion projection:
//
//	{v: {foo: 1, bar: 1}}               -> {v: {foo: 1}}
//	{v: {bar: 1}}                       -> {v: {}}
//	{v: [{bar: 1}]}                     -> {v: [{}]}
//	{v: [{foo: 1}, {foo: 2}, {bar: 1}]} -> {v: [{foo: 1}, {foo: 2}, {}]}
//
// Example: "v.0.foo" path inclusion projection:
//
//	{v: [{foo: 1}, {foo: 2}, {bar: 1}]} -> {v: [{}, {}, {}]}
//
// Example: "v.$" positional projection with filter {v: float64(2)}:
//
//	{v: [int64(1), int64(2), int32(2)]} -> {v: [int64(2)]}
func includeProjection(path types.Path, curIndex int, source any, projected, filter *types.Document) (*types.Array, error) {
	key := path.Slice()[curIndex]

	switch source := source.(type) {
	case *types.Document:
		embeddedSource, err := source.Get(key)
		if err != nil {
			// key does not exist, nothing to set.
			return nil, nil
		}

		if path.Len()-1 <= curIndex {
			// next index is suffix, set field in projected.
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
				// if array is at next index, pass it to allow array to use existing
				// projection fields.
				doc = must.NotFail(types.NewDocument(path.Slice()[curIndex+1], arr))
			}
		}

		if path.TrimPrefix().Prefix() == "$" {
			// positional projection sets the value for non array field.
			projected.Set(key, embeddedSource)
		}

		// when next index has an array use returned value arr,
		// if it has a document, field in the doc is set by includeProjection.
		arr, err := includeProjection(path, curIndex+1, embeddedSource, doc, filter)
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
		if key == "$" {
			v, err := getPositionalProjection(source, filter, path.String())
			if err != nil {
				return nil, err
			}

			return v, nil
		}

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

				v, _ = arr.Get(i)
				docVal, ok := v.(*types.Document)
				if !ok {
					panic("projected field must be a document")
				}

				doc = docVal
			} else {
				// first inclusion field, insert it to the doc.
				arr.Append(doc)
			}

			if _, err = includeProjection(path, curIndex, arrElem, doc, filter); err != nil {
				return nil, err
			}

			arr.Set(i, doc)
			i++
		}

		if path.Suffix() == "$" && arr.Len() == 0 {
			// positional projection only handles one array at the suffix,
			// path prefixes cannot contain array.
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrElementMismatchPositionalProjection,
				"Executor error during find command :: caused by :: positional operator '.$' element mismatch",
				"projection",
			)
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
// Examples: "v.foo" path exclusion projection:
//
//	{v: {foo: 1}}                       -> {v: {}}
//	{v: {foo: 1, bar: 1}}               -> {v: {bar: 1}}
//	{v: [{foo: 1}, {foo: 2}]}           -> {v: [{}, {}]}
//	{v: [{foo: 1}, {foo: 2}, {bar: 1}]} -> {v: [{}, {}, {bar: 1}]}
//
// Example: "v.0.foo" path exclusion projection:
//
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

