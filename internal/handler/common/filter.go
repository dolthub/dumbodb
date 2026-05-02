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
	"math"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// FilterDocument returns true if given document satisfies given filter expression.
//
// Passed arguments must not be modified.
func FilterDocument(doc, filter *types.Document) (bool, error) {
	iter := filter.Iterator()
	defer iter.Close()

	for {
		filterKey, filterValue, err := iter.Next()
		if err != nil {
			if errors.Is(err, iterator.ErrIteratorDone) {
				return true, nil
			}

			return false, lazyerrors.Error(err)
		}

		// top-level filters are ANDed together
		matches, err := filterDocumentPair(doc, filterKey, filterValue)
		if err != nil {
			return false, lazyerrors.Error(err)
		}
		if !matches {
			return false, nil
		}
	}
}

// filterDocumentPair handles a single filter element key/value pair {filterKey: filterValue}.
func filterDocumentPair(doc *types.Document, filterKey string, filterValue any) (bool, error) {
	var vals []any
	filterSuffix := filterKey

	if strings.ContainsRune(filterKey, '.') {
		path, err := types.NewPathFromString(filterKey)
		if err != nil {
			return false, lazyerrors.Error(err)
		}

		filterSuffix = path.Suffix()

		// filter using dot notation returns the value by valid array index
		// or values for the given key in array's document
		if vals, err = commonpath.FindValues(doc, path, &commonpath.FindValuesOpts{
			FindArrayIndex:     true,
			FindArrayDocuments: true,
		}); err != nil {
			return false, lazyerrors.Error(err)
		}
	} else {
		if val, _ := doc.Get(filterKey); val != nil {
			vals = []any{val}
		}
	}

	if strings.HasPrefix(filterKey, "$") {
		// {$operator: filterValue}
		return filterOperator(doc, filterKey, filterValue)
	}

	switch filterValue := filterValue.(type) {
	case *types.Document:
		var docs []*types.Document
		for _, val := range vals {
			docs = append(docs, must.NotFail(types.NewDocument(filterSuffix, val)))
		}

		if len(docs) == 0 {
			// operators like $nin uses empty document to filter non-existent field
			docs = append(docs, types.MakeDocument(0))
		}

		for _, doc := range docs {
			// {field: {expr}} or {field: {document}}
			ok, err := filterFieldExpr(doc, filterKey, filterSuffix, filterValue)
			if err != nil {
				return false, err
			}

			if ok {
				return true, nil
			}
		}
	case types.NullType:
		if len(vals) == 0 {
			// comparing non-existent field with null returns true
			return true, nil
		}

		for _, val := range vals {
			if result := types.Compare(val, filterValue); result == types.Equal {
				return true, nil
			}
		}
	case types.Regex:
		for _, val := range vals {
			ok, err := filterFieldRegex(val, filterValue)
			if err != nil {
				return false, err
			}

			if ok {
				return true, nil
			}
		}
	default:
		for _, val := range vals {
			if result := types.Compare(val, filterValue); result == types.Equal {
				return true, nil
			}
		}
	}

	// If we got here, it means that none of the documents matched the filter.
	return false, nil
}

// filterOperator handles a top-level operator filter {$operator: filterValue}.
func filterOperator(doc *types.Document, operator string, filterValue any) (bool, error) {
	switch operator {
	case "$and":
		// {$and: [{expr1}, {expr2}, ...]}
		exprs, ok := filterValue.(*types.Array)
		if !ok {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$and must be an array",
				operator,
			)
		}

		if exprs.Len() == 0 {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$and/$or/$nor must be a nonempty array",
				operator,
			)
		}

		for i := 0; i < exprs.Len(); i++ {
			_, ok := must.NotFail(exprs.Get(i)).(*types.Document)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$or/$and/$nor entries need to be full objects",
					operator,
				)
			}
		}

		for i := 0; i < exprs.Len(); i++ {
			expr := must.NotFail(exprs.Get(i)).(*types.Document)

			matches, err := FilterDocument(doc, expr)
			if err != nil {
				return false, err
			}
			if !matches {
				return false, nil
			}
		}

		return true, nil

	case "$or":
		// {$or: [{expr1}, {expr2}, ...]}
		exprs, ok := filterValue.(*types.Array)
		if !ok {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$or must be an array",
				operator,
			)
		}

		if exprs.Len() == 0 {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$and/$or/$nor must be a nonempty array",
				operator,
			)
		}

		for i := 0; i < exprs.Len(); i++ {
			_, ok := must.NotFail(exprs.Get(i)).(*types.Document)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$or/$and/$nor entries need to be full objects",
					operator,
				)
			}
		}

		for i := 0; i < exprs.Len(); i++ {
			expr := must.NotFail(exprs.Get(i)).(*types.Document)

			matches, err := FilterDocument(doc, expr)
			if err != nil {
				return false, err
			}
			if matches {
				return true, nil
			}
		}

		return false, nil

	case "$nor":
		// {$nor: [{expr1}, {expr2}, ...]}
		exprs, ok := filterValue.(*types.Array)
		if !ok {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$nor must be an array",
				operator,
			)
		}

		if exprs.Len() == 0 {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$and/$or/$nor must be a nonempty array",
				operator,
			)
		}

		for i := 0; i < exprs.Len(); i++ {
			_, ok := must.NotFail(exprs.Get(i)).(*types.Document)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$or/$and/$nor entries need to be full objects",
					operator,
				)
			}
		}

		for i := 0; i < exprs.Len(); i++ {
			expr := must.NotFail(exprs.Get(i)).(*types.Document)

			matches, err := FilterDocument(doc, expr)
			if err != nil {
				return false, err
			}
			if matches {
				return false, nil
			}
		}

		return true, nil

	case "$text":
		// {$text: {$search: "word1 word2", $language: "en", $caseSensitive: false, $diacriticSensitive: false}}
		textDoc, ok := filterValue.(*types.Document)
		if !ok {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$text expects an object",
				operator,
			)
		}

		return filterTextSearch(doc, textDoc)

	case "$comment":
		return true, nil

	case "$expr":
		return filterExprOperator(doc, must.NotFail(types.NewDocument(operator, filterValue)))

	case "$jsonSchema":
		// {$jsonSchema: <JSON Schema object>}
		schema, ok := filterValue.(*types.Document)
		if !ok {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$jsonSchema must be a valid JSON object",
				"$jsonSchema",
			)
		}

		return filterJSONSchema(doc, schema)

	default:
		msg := fmt.Sprintf(
			`unknown top level operator: %s. `+
				`If you have a field name that starts with a '$' symbol, consider using $getField or $setField.`,
			operator,
		)

		return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, msg, "$operator")
	}
}

// filterExprOperator uses $expr operator to allow usage of aggregation expression.
// It returns boolean indicating filter has matched.
//
// $expr is primary used by operators such as $gt and $cond which return boolean result.
// However, if non-boolean result is returned from processing aggregation expression,
// it returns false for null or zero value and true for all other values.
func filterExprOperator(doc, filter *types.Document) (bool, error) {
	op, err := operators.NewExpr(filter, "$expr")
	if err != nil {
		return false, err
	}

	v, err := op.Process(doc)
	if err != nil {
		return false, lazyerrors.Error(err)
	}

	switch v := v.(type) {
	case *types.Document, *types.Array, string, types.Binary, types.ObjectID, time.Time, types.Regex, types.Timestamp:
		return true, nil
	case float64, int32, int64:
		return types.Compare(v, int32(0)) != types.Equal, nil
	case bool:
		return v, nil
	case types.NullType:
		return false, nil
	default:
		panic(fmt.Sprintf("common.filterExprOperator: unexpected type %[1]T (%#[1]v)", v))
	}
}

// filterFieldExpr handles {field: {expr}} or {field: {document}} filter.
func filterFieldExpr(doc *types.Document, filterKey, filterSuffix string, expr *types.Document) (bool, error) {
	// check if both documents are empty
	if expr.Len() == 0 {
		fieldValue, err := doc.Get(filterSuffix)
		if err != nil {
			return false, nil
		}
		if fieldValue, ok := fieldValue.(*types.Document); ok && fieldValue.Len() == 0 {
			return true, nil
		}
		return false, nil
	}

	for _, exprKey := range expr.Keys() {
		if exprKey == "$options" {
			// handled by $regex
			continue
		}

		exprValue := must.NotFail(expr.Get(exprKey))

		fieldValue, err := doc.Get(filterSuffix)
		if err != nil {
			switch exprKey {
			case "$exists", "$not", "$elemMatch":
			case "$type":
				if v, ok := exprValue.(string); ok && v == "null" {
					// null and unset are different for $type operator.
					return false, nil
				}
			default:
				// Set non-existent field to null for the operator
				// to compute result. The comparison treats non-existent
				// field on documents as equivalent.
				fieldValue = types.Null
			}
		}

		if !strings.HasPrefix(exprKey, "$") {
			if documentValue, ok := fieldValue.(*types.Document); ok {
				result := types.Compare(documentValue, expr)
				return result == types.Equal, nil
			}
			return false, nil
		}

		switch exprKey {
		case "$eq":
			// {field: {$eq: exprValue}}
			switch exprValue := exprValue.(type) {
			case *types.Document:
				if fieldValue, ok := fieldValue.(*types.Document); ok {
					result := types.Compare(exprValue, fieldValue)
					return result == types.Equal, nil
				}
				return false, nil
			default:
				result := types.Compare(fieldValue, exprValue)
				if result != types.Equal {
					return false, nil
				}
			}

		case "$ne":
			// {field: {$ne: exprValue}}
			switch exprValue := exprValue.(type) {
			case *types.Document:
				if fieldValue, ok := fieldValue.(*types.Document); ok {
					result := types.Compare(exprValue, fieldValue)
					return result != types.Equal, nil
				}

				return true, nil
			case types.Regex:
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"Can't have regex as arg to $ne.",
					exprKey,
				)
			default:
				result := types.Compare(fieldValue, exprValue)
				if result == types.Equal {
					return false, nil
				}
			}

		case "$gt":
			// {field: {$gt: exprValue}}
			if _, ok := exprValue.(types.Regex); ok {
				msg := fmt.Sprintf(`Can't have RegEx as arg to predicate over field '%s'.`, filterKey)
				return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, msg, exprKey)
			}

			// Array and non-array comparison with $gt compares the non-array
			// value against the maximum value of the same BSON type value of the array.
			// Filter the array by only keeping the same type as the non-array value,
			// then find the maximum value from the array.
			// If array does not contain same BSON type, returns false.
			// All numbers are treated as the same type.
			// Example:
			// expr {v: {$gt: 42}}
			// value [{v: 40}, {v: 41.5}, {v: "foo"}, {v: nil}]
			// Above compares the maximum number of array 41.5 to the filter 42,
			// and results in Less. Other values "foo" and nil which are
			// not number type are not considered for $gt comparison.

			result := types.CompareOrderForOperator(fieldValue, exprValue, types.Descending)
			if result != types.Greater {
				return false, nil
			}

		case "$gte":
			// {field: {$gte: exprValue}}
			if _, ok := exprValue.(types.Regex); ok {
				msg := fmt.Sprintf(`Can't have RegEx as arg to predicate over field '%s'.`, filterKey)
				return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, msg, exprKey)
			}

			// Array and non-array comparison with $gte compares the non-array
			// value against the maximum value of the same BSON type value of the array.
			// Filter the array by only keeping the same type as the non-array value,
			// then find the maximum value from the array.
			// If array does not contain same BSON type, returns false.
			// All numbers are treated as the same type.
			// Example:
			// expr {v: {$gte: 42}}
			// value [{v: 40}, {v: 41.5}, {v: "foo"}, {v: nil}]
			// Above compares the maximum number of array 41.5 to the filter 42,
			// and results in Less. Other values "foo" and nil which are
			// not number type are not considered for $gte comparison.
			result := types.CompareOrderForOperator(fieldValue, exprValue, types.Descending)
			if result != types.Equal && result != types.Greater {
				return false, nil
			}

		case "$lt":
			// {field: {$lt: exprValue}}
			if _, ok := exprValue.(types.Regex); ok {
				msg := fmt.Sprintf(`Can't have RegEx as arg to predicate over field '%s'.`, filterKey)
				return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, msg, exprKey)
			}

			// Array and non-array comparison with $lt compares the non-array
			// value against the minimum value of the same BSON type value of the array.
			// Filter the array by only keeping the same type as the non-array value,
			// then find the minimum value from the array.
			// If array does not contain same BSON type, returns false.
			// All numbers are treated as the same type.
			// Example:
			// expr {v: {$gte: 42}}
			// value [{v: 40}, {v: 41.5}, {v: "foo"}, {v: nil}]
			// Above compares the minimum number of array 40 to the filter 42,
			// and results in Less. Other values "foo" and nil which are
			// not number type are not considered for $lt comparison.

			result := types.CompareOrderForOperator(fieldValue, exprValue, types.Ascending)
			if result != types.Less {
				return false, nil
			}

		case "$lte":
			// {field: {$lte: exprValue}}
			if _, ok := exprValue.(types.Regex); ok {
				msg := fmt.Sprintf(`Can't have RegEx as arg to predicate over field '%s'.`, filterKey)
				return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, msg, exprKey)
			}

			// Array and non-array comparison with $lte compares the non-array
			// value against the minimum value of the same BSON type value of the array.
			// Filter the array by only keeping the same type as the non-array value,
			// then find the minimum value from the array.
			// If array does not contain same BSON type, returns false.
			// All numbers are treated as the same type.
			// Example:
			// expr {v: {$gte: 42}}
			// value [{v: 40}, {v: 41.5}, {v: "foo"}, {v: nil}]
			// Above compares the minimum number of array 40 to the filter 42,
			// and results in Less. Other values "foo" and nil which are
			// not number type are not considered for $lt comparison.

			result := types.CompareOrderForOperator(fieldValue, exprValue, types.Ascending)
			if result != types.Equal && result != types.Less {
				return false, nil
			}

		case "$in":
			// {field: {$in: [value1, value2, ...]}}
			arr, ok := exprValue.(*types.Array)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$in needs an array",
					exprKey,
				)
			}

			var found bool
			for i := 0; i < arr.Len(); i++ {
				if found {
					break
				}

				switch arrValue := must.NotFail(arr.Get(i)).(type) {
				case *types.Document:
					for _, key := range arrValue.Keys() {
						if strings.HasPrefix(key, "$") {
							return false, handlererrors.NewCommandErrorMsgWithArgument(
								handlererrors.ErrBadValue,
								"cannot nest $ under $in",
								exprKey,
							)
						}
					}

					if fieldValue, ok := fieldValue.(*types.Document); ok {
						if result := types.Compare(fieldValue, arrValue); result == types.Equal {
							found = true
						}
					}
				case types.Regex:
					match, err := filterFieldRegex(fieldValue, arrValue)
					switch {
					case err != nil:
						return false, err
					case match:
						found = true
					}
				default:
					result := types.Compare(fieldValue, arrValue)
					if result == types.Equal {
						found = true
					}
				}
			}

			if !found {
				return false, nil
			}

		case "$nin":
			// {field: {$nin: [value1, value2, ...]}}
			arr, ok := exprValue.(*types.Array)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$nin needs an array",
					exprKey,
				)
			}

			var found bool
			for i := 0; i < arr.Len(); i++ {
				if found {
					break
				}

				switch arrValue := must.NotFail(arr.Get(i)).(type) {
				case *types.Document:
					for _, key := range arrValue.Keys() {
						if strings.HasPrefix(key, "$") {
							return false, handlererrors.NewCommandErrorMsgWithArgument(
								handlererrors.ErrBadValue,
								"cannot nest $ under $in",
								exprKey,
							)
						}
					}

					if fieldValue, ok := fieldValue.(*types.Document); ok {
						if result := types.Compare(fieldValue, arrValue); result == types.Equal {
							found = true
						}
					}
				case types.Regex:
					match, err := filterFieldRegex(fieldValue, arrValue)
					switch {
					case err != nil:
						return false, err
					case match:
						found = true
					}
				default:
					result := types.Compare(fieldValue, arrValue)
					if result == types.Equal {
						found = true
					}
				}
			}

			if found {
				return false, nil
			}

		case "$not":
			// {field: {$not: {expr}}}
			switch exprValue := exprValue.(type) {
			case *types.Document:
				res, err := filterFieldExpr(doc, filterKey, filterSuffix, exprValue)
				if res || err != nil {
					return false, err
				}
			case types.Regex:
				optionsAny, _ := expr.Get("$options")
				res, err := filterFieldExprRegex(fieldValue, exprValue, optionsAny)
				if res || err != nil {
					return false, err
				}
			default:
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$not needs a regex or a document",
					exprKey,
				)
			}

		case "$regex":
			// {field: {$regex: exprValue}}
			optionsAny, _ := expr.Get("$options")
			res, err := filterFieldExprRegex(fieldValue, exprValue, optionsAny)
			if !res || err != nil {
				return false, err
			}

		case "$elemMatch":
			// {field: {$elemMatch: value}}
			res, err := filterFieldExprElemMatch(doc, filterKey, filterSuffix, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$size":
			// {field: {$size: value}}
			res, err := filterFieldExprSize(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$all":
			// {field: {$all: [value, another_value, ...]}}
			res, err := filterFieldExprAll(doc, filterKey, filterSuffix, fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$bitsAllClear":
			// {field: {$bitsAllClear: value}}
			res, err := filterFieldExprBitsAllClear(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$bitsAllSet":
			// {field: {$bitsAllSet: value}}
			res, err := filterFieldExprBitsAllSet(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$bitsAnyClear":
			// {field: {$bitsAnyClear: value}}
			res, err := filterFieldExprBitsAnyClear(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$bitsAnySet":
			// {field: {$bitsAnySet: value}}
			res, err := filterFieldExprBitsAnySet(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$mod":
			// {field: {$mod: [divisor, remainder]}}
			res, err := filterFieldMod(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$exists":
			// {field: {$exists: value}}
			res, err := filterFieldExprExists(fieldValue != nil, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$type":
			// {field: {$type: value}}
			res, err := filterFieldExprType(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$geoWithin":
			// {field: {$geoWithin: spec}}
			spec, ok := exprValue.(*types.Document)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$geoWithin requires a document argument",
					"$geoWithin",
				)
			}
			res, err := filterFieldGeoWithin(fieldValue, spec)
			if !res || err != nil {
				return false, err
			}

		case "$geoIntersects":
			// {field: {$geoIntersects: {$geometry: ...}}}
			spec, ok := exprValue.(*types.Document)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$geoIntersects requires a document argument",
					"$geoIntersects",
				)
			}
			res, err := filterFieldGeoIntersects(fieldValue, spec)
			if !res || err != nil {
				return false, err
			}

		case "$near":
			// {field: {$near: {$geometry: ..., $maxDistance: n, $minDistance: n}}}
			// or legacy: {field: {$near: [lon, lat]}} with optional sibling $maxDistance
			switch nv := exprValue.(type) {
			case *types.Document:
				res, err := filterFieldNear(fieldValue, nv, false)
				if !res || err != nil {
					return false, err
				}
			case *types.Array:
				if nv.Len() < 2 {
					return false, nil
				}
				lon, e := toFloat64(must.NotFail(nv.Get(0)))
				if e != nil {
					return false, nil
				}
				lat, e := toFloat64(must.NotFail(nv.Get(1)))
				if e != nil {
					return false, nil
				}
				coordArr := must.NotFail(types.NewArray(lon, lat))
				ptDoc := must.NotFail(types.NewDocument(
					"type", "Point",
					"coordinates", coordArr,
				))
				nearDoc := must.NotFail(types.NewDocument("$geometry", ptDoc))
				if maxAny, e2 := expr.Get("$maxDistance"); e2 == nil {
					nearDoc.Set("$maxDistance", maxAny)
				}
				res, err := filterFieldNear(fieldValue, nearDoc, false)
				if !res || err != nil {
					return false, err
				}
			default:
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$near requires a document or array argument",
					"$near",
				)
			}

		case "$nearSphere":
			// Same as $near but always uses spherical (Haversine) distance.
			switch nv := exprValue.(type) {
			case *types.Document:
				res, err := filterFieldNear(fieldValue, nv, true)
				if !res || err != nil {
					return false, err
				}
			case *types.Array:
				if nv.Len() < 2 {
					return false, nil
				}
				lon, e := toFloat64(must.NotFail(nv.Get(0)))
				if e != nil {
					return false, nil
				}
				lat, e := toFloat64(must.NotFail(nv.Get(1)))
				if e != nil {
					return false, nil
				}
				coordArr := must.NotFail(types.NewArray(lon, lat))
				ptDoc := must.NotFail(types.NewDocument(
					"type", "Point",
					"coordinates", coordArr,
				))
				nearDoc := must.NotFail(types.NewDocument("$geometry", ptDoc))
				// For $nearSphere with legacy 2d-index coordinates, $maxDistance and
				// $minDistance are specified in radians (not meters). Convert to meters
				// because filterFieldNear uses haversineMeters for distance comparison.
				if maxAny, e2 := expr.Get("$maxDistance"); e2 == nil {
					if maxRad, e3 := toFloat64(maxAny); e3 == nil {
						nearDoc.Set("$maxDistance", maxRad*earthRadiusMeters)
					}
				}
				if minAny, e2 := expr.Get("$minDistance"); e2 == nil {
					if minRad, e3 := toFloat64(minAny); e3 == nil {
						nearDoc.Set("$minDistance", minRad*earthRadiusMeters)
					}
				}
				res, err := filterFieldNear(fieldValue, nearDoc, true)
				if !res || err != nil {
					return false, err
				}
			default:
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$nearSphere requires a document or array argument",
					"$nearSphere",
				)
			}

		// $maxDistance and $minDistance are consumed by $near/$nearSphere above.
		case "$maxDistance", "$minDistance":
			// skip  -- handled as sibling keys within $near / $nearSphere

		default:
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("unknown operator: %s", exprKey),
				"$operator",
			)
		}
	}

	return true, nil
}

// filterFieldRegex handles {field: /regex/} filter. Provides regular expression capabilities
// for pattern matching strings in queries, even if the strings are in an array.
func filterFieldRegex(fieldValue any, regex types.Regex) (bool, error) {
	for _, option := range regex.Options {
		if !slices.Contains([]rune{'i', 'm', 's', 'x'}, option) {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadRegexOption,
				fmt.Sprintf(" invalid flag in regex options: %c", option),
				"$options",
			)
		}
	}

	re, err := regex.Compile()
	if err != nil {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrRegexMissingParen,
			err.Error(),
			"$regex",
		)
	}

	switch fieldValue := fieldValue.(type) {
	case *types.Array:
		for i := 0; i < fieldValue.Len(); i++ {
			arrValue := must.NotFail(fieldValue.Get(i))
			s, isString := arrValue.(string)
			if !isString {
				continue
			}
			if re.MatchString(s) {
				return true, nil
			}
		}

	case string:
		return re.MatchString(fieldValue), nil

	case types.Regex:
		result := types.Compare(fieldValue, regex)
		return result == types.Equal, nil
	}

	return false, nil
}

// filterFieldExprRegex handles {field: {$regex: regexValue, $options: optionsValue}} filter.
func filterFieldExprRegex(fieldValue any, regexValue, optionsValue any) (bool, error) {
	var options string
	if optionsValue != nil {
		var ok bool
		if options, ok = optionsValue.(string); !ok {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$options has to be a string",
				"$options",
			)
		}
	}

	switch regexValue := regexValue.(type) {
	case string:
		regex := types.Regex{
			Pattern: regexValue,
			Options: options,
		}
		return filterFieldRegex(fieldValue, regex)

	case types.Regex:
		if options != "" {
			if regexValue.Options != "" {
				return false, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrRegexOptions,
					"options set in both $regex and $options",
				)
			}
			regexValue.Options = options
		}
		return filterFieldRegex(fieldValue, regexValue)

	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$regex has to be a string",
			"$regex",
		)
	}
}

// filterFieldExprSize handles {field: {$size: sizeValue}} filter.
func filterFieldExprSize(fieldValue any, sizeValue any) (bool, error) {
	size, err := handlerparams.GetWholeNumberParam(sizeValue)
	if err != nil {
		switch err {
		case handlerparams.ErrUnexpectedType:
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf(`Failed to parse $size. Expected a number in: $size: %s`, types.FormatAnyValue(sizeValue)),
				"$size",
			)
		case handlerparams.ErrNotWholeNumber:
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf(`Failed to parse $size. Expected an integer: $size: %s`, types.FormatAnyValue(sizeValue)),
				"$size",
			)
		case handlerparams.ErrInfinity:
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf(
					`Failed to parse $size. Cannot represent as a 64-bit integer: $size: %s`,
					types.FormatAnyValue(sizeValue),
				),
				"$size",
			)
		default:
			return false, err
		}
	}

	if size < 0 {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf(
				`Failed to parse $size. Expected a non-negative number in: $size: %s`,
				types.FormatAnyValue(sizeValue),
			),
			"$size",
		)
	}

	arr, ok := fieldValue.(*types.Array)
	if !ok {
		return false, nil
	}

	if arr.Len() != int(size) {
		return false, nil
	}

	return true, nil
}

// filterFieldExprAll handles {field: {$all: [value, another_value, ...]}} filter.
// The main purpose of $all is to filter arrays.
// It is possible to filter non-arrays: {field: {$all: [value]}}, but such statement is equivalent to {field: value}.
//
// Special case: if any query element is {$elemMatch: expr}, it is treated as an $elemMatch
// condition applied to the field array (MongoDB extension: $all + $elemMatch).
func filterFieldExprAll(doc *types.Document, filterKey, filterSuffix string, fieldValue any, allValue any) (bool, error) {
	query, ok := allValue.(*types.Array)
	if !ok {
		return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, "$all needs an array", "$all")
	}

	if query.Len() == 0 {
		return false, nil
	}

	// Check if any query element is an {$elemMatch: ...} expression.
	// In that case, $all with $elemMatch has special semantics: for each
	// {$elemMatch: expr} in the query list, the field array must have at least
	// one element matching the expression.
	hasElemMatch := false
	for i := 0; i < query.Len(); i++ {
		elem := must.NotFail(query.Get(i))
		if d, ok := elem.(*types.Document); ok {
			if _, err := d.Get("$elemMatch"); err == nil {
				hasElemMatch = true
				break
			}
		}
	}

	if hasElemMatch {
		// For $all with $elemMatch elements, process each query element:
		// - {$elemMatch: expr} → field array must contain an element matching expr
		// - other values → not supported in mixed mode; treat as no-match
		for i := 0; i < query.Len(); i++ {
			elem := must.NotFail(query.Get(i))
			elemDoc, ok := elem.(*types.Document)
			if !ok {
				return false, nil
			}
			elemMatchValue, err := elemDoc.Get("$elemMatch")
			if err != nil {
				return false, nil
			}
			matched, err := filterFieldExprElemMatch(doc, filterKey, filterSuffix, elemMatchValue)
			if err != nil {
				return false, err
			}
			if !matched {
				return false, nil
			}
		}
		return true, nil
	}

	switch value := fieldValue.(type) {
	case *types.Document:
		// For documents we return false as $all doesn't work on documents.
		return false, nil

	case *types.Array:
		// For arrays we check that the array contains all the elements of the query.
		return value.ContainsAll(query), nil

	default:
		// For other types (scalars) we check that the value is equal to each scalar in the query.
		// Example: value: 42, query: [42, 42] should give us `true`
		for i := 0; i < query.Len(); i++ {
			res := types.Compare(value, must.NotFail(query.Get(i)))
			if res != types.Equal {
				return false, nil
			}
		}
		return true, nil
	}
}

// filterFieldExprBitsAllClear handles {field: {$bitsAllClear: value}} filter.
func filterFieldExprBitsAllClear(fieldValue, maskValue any) (bool, error) {
	bitmask, err := getBinaryMaskParam("$bitsAllClear", maskValue)
	if err != nil {
		return false, err
	}

	switch value := fieldValue.(type) {
	case float64:
		if isInvalidBitwiseValue(value) {
			return false, nil
		}

		return (^uint64(value) & bitmask) == bitmask, nil

	case types.Binary:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			"BinData() not supported yet",
			"$bitsAllClear",
		)

	case int32:
		return (^uint64(value) & bitmask) == bitmask, nil

	case int64:
		return (^uint64(value) & bitmask) == bitmask, nil

	case types.Decimal128:
		intVal, ok := decimal128ToInt64(value)
		if !ok {
			return false, nil
		}
		return (^uint64(intVal) & bitmask) == bitmask, nil

	default:
		return false, nil
	}
}

// filterFieldExprBitsAllSet handles {field: {$bitsAllSet: value}} filter.
func filterFieldExprBitsAllSet(fieldValue, maskValue any) (bool, error) {
	bitmask, err := getBinaryMaskParam("$bitsAllSet", maskValue)
	if err != nil {
		return false, err
	}

	switch value := fieldValue.(type) {
	case float64:
		if isInvalidBitwiseValue(value) {
			return false, nil
		}

		return (uint64(value) & bitmask) == bitmask, nil

	case types.Binary:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			"BinData() not supported yet",
			"$bitsAllSet",
		)

	case int32:
		return (uint64(value) & bitmask) == bitmask, nil

	case int64:
		return (uint64(value) & bitmask) == bitmask, nil

	case types.Decimal128:
		intVal, ok := decimal128ToInt64(value)
		if !ok {
			return false, nil
		}
		return (uint64(intVal) & bitmask) == bitmask, nil

	default:
		return false, nil
	}
}

// filterFieldExprBitsAnyClear handles {field: {$bitsAnyClear: value}} filter.
func filterFieldExprBitsAnyClear(fieldValue, maskValue any) (bool, error) {
	bitmask, err := getBinaryMaskParam("$bitsAnyClear", maskValue)
	if err != nil {
		return false, err
	}

	switch value := fieldValue.(type) {
	case float64:
		if isInvalidBitwiseValue(value) {
			return false, nil
		}

		return (^uint64(value) & bitmask) != 0, nil

	case types.Binary:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			"BinData() not supported yet",
			"$bitsAnyClear",
		)

	case int32:
		return (^uint64(value) & bitmask) != 0, nil

	case int64:
		return (^uint64(value) & bitmask) != 0, nil

	case types.Decimal128:
		intVal, ok := decimal128ToInt64(value)
		if !ok {
			return false, nil
		}
		return (^uint64(intVal) & bitmask) != 0, nil

	default:
		return false, nil
	}
}

// filterFieldExprBitsAnySet handles {field: {$bitsAnySet: value}} filter.
func filterFieldExprBitsAnySet(fieldValue, maskValue any) (bool, error) {
	bitmask, err := getBinaryMaskParam("$bitsAnySet", maskValue)
	if err != nil {
		return false, err
	}

	switch value := fieldValue.(type) {
	case float64:
		if isInvalidBitwiseValue(value) {
			return false, nil
		}

		return (uint64(value) & bitmask) != 0, nil

	case types.Binary:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			"BinData() not supported yet",
			"$bitsAnySet",
		)

	case int32:
		return (uint64(value) & bitmask) != 0, nil

	case int64:
		return (uint64(value) & bitmask) != 0, nil

	case types.Decimal128:
		intVal, ok := decimal128ToInt64(value)
		if !ok {
			return false, nil
		}
		return (uint64(intVal) & bitmask) != 0, nil

	default:
		return false, nil
	}
}

// isInvalidBitwiseValue returns true for an invalid value of float64
// use for bitwise operation.
// Non-integer float64, Nan, Inf are unsupported.
// The value less than math.MaxInt64,
// and greater than or equal to math.MinInt64 are unsupported.
func isInvalidBitwiseValue(value float64) bool {
	return value != math.Trunc(value) ||
		math.IsNaN(value) ||
		math.IsInf(value, 0) ||
		value >= math.MaxInt64 ||
		value < math.MinInt64
}

// decimal128ToInt64 attempts to convert a types.Decimal128 value to int64 for
// use in bitwise operations. It truncates toward zero (like MongoDB does).
// Returns (value, true) on success, or (0, false) if the value cannot be
// represented as an int64 (NaN, Inf, out of range).
func decimal128ToInt64(d types.Decimal128) (int64, bool) {
	// Use bson.Decimal128 which stores (H=high, L=low).
	// types.Decimal128 stores H and L fields matching the wire format.
	p := bson.NewDecimal128(d.H, d.L)

	bi, exp, err := p.BigInt()
	if err != nil {
		// NaN or Inf
		return 0, false
	}

	// Apply the exponent: value = bi * 10^exp
	if exp > 0 {
		factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
		bi = new(big.Int).Mul(bi, factor)
	} else if exp < 0 {
		// Truncate toward zero: divide and discard remainder
		divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-exp)), nil)
		// Truncate toward zero: use Quo (truncated division)
		bi = new(big.Int).Quo(bi, divisor)
	}

	// Check if it fits in int64
	minInt64 := new(big.Int).SetInt64(math.MinInt64)
	maxInt64 := new(big.Int).SetInt64(math.MaxInt64)
	if bi.Cmp(minInt64) < 0 || bi.Cmp(maxInt64) > 0 {
		return 0, false
	}

	return bi.Int64(), true
}

// filterFieldMod handles {field: {$mod: [divisor, remainder]}} filter.
func filterFieldMod(fieldValue, exprValue any) (bool, error) {
	arr := exprValue.(*types.Array)
	if arr.Len() < 2 {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			`malformed mod, not enough elements`,
			"$mod",
		)
	}
	if arr.Len() > 2 {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			`malformed mod, too many elements`,
			"$mod",
		)
	}

	var field, divisor, remainder int64
	switch d := must.NotFail(arr.Get(0)).(type) {
	case float64:
		if math.IsNaN(d) || math.IsInf(d, 0) {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				`malformed mod, divisor value is invalid :: caused by :: `+`Unable to coerce NaN/Inf to integral type`,
				"$mod",
			)
		}

		d = math.Trunc(d)
		if d >= float64(math.MaxInt64) || d < float64(math.MinInt64) {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				`malformed mod, divisor value is invalid :: caused by :: `+`Out of bounds coercing to integral value`,
				"$mod",
			)
		}

		divisor = int64(d)

	case int32:
		divisor = int64(d)

	case int64:
		divisor = d

	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			`malformed mod, divisor not a number`,
			"$mod",
		)
	}

	switch r := must.NotFail(arr.Get(1)).(type) {
	case float64:
		if math.IsNaN(r) || math.IsInf(r, 0) {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				`malformed mod, remainder value is invalid :: caused by :: `+
					`Unable to coerce NaN/Inf to integral type`, "$mod",
			)
		}

		r = math.Trunc(r)

		if r >= float64(math.MaxInt64) || r < float64(math.MinInt64) {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				`malformed mod, remainder value is invalid :: caused by :: `+
					`Out of bounds coercing to integral value`, "$mod",
			)
		}

		remainder = int64(r)

	case int32:
		remainder = int64(r)

	case int64:
		remainder = r

	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			`malformed mod, remainder not a number`,
			"$mod",
		)
	}

	if divisor == 0 {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			`divisor cannot be 0`,
			"$mod",
		)
	}

	// For array fields, check if any element satisfies the modulo condition.
	if arr, ok := fieldValue.(*types.Array); ok {
		for i := 0; i < arr.Len(); i++ {
			elem := must.NotFail(arr.Get(i))
			ok, err := filterFieldMod(elem, exprValue)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}

	switch f := fieldValue.(type) {
	case float64:
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false, nil
		}
		f = math.Trunc(f)
		field = int64(f)

		if f != float64(field) {
			return false, nil
		}

	case int32:
		field = int64(f)

	case int64:
		field = f

	default:
		return false, nil
	}

	f := field % divisor
	if f != remainder {
		return false, nil
	}

	return true, nil
}

// filterFieldExprExists handles {field: {$exists: value}} filter.
func filterFieldExprExists(fieldExist bool, exprValue any) (bool, error) {
	expr, ok := exprValue.(bool)
	// return all documents if filter value is not bool type
	if !ok {
		return true, nil
	}

	switch {
	case fieldExist && expr:
		return true, nil
	case !fieldExist && !expr:
		return true, nil
	default:
		return false, nil
	}
}

// filterFieldExprType handles {field: {$type: value}} filter.
func filterFieldExprType(fieldValue, exprValue any) (bool, error) {
	switch exprValue := exprValue.(type) {
	case *types.Array:
		hasSameType := handlerparams.HasSameTypeElements(exprValue)

		for i := 0; i < exprValue.Len(); i++ {
			exprValue := must.NotFail(exprValue.Get(i))

			switch exprValue := exprValue.(type) {
			case float64:
				if math.IsNaN(exprValue) || math.IsInf(exprValue, 0) {
					return false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						`Invalid numerical type code: `+strings.Trim(strings.ToLower(fmt.Sprintf("%v", exprValue)), "+"),
						"$type",
					)
				}
				if exprValue != math.Trunc(exprValue) {
					return false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						fmt.Sprintf(`Invalid numerical type code: %v`, exprValue),
						"$type",
					)
				}

				code, err := handlerparams.NewTypeCode(int32(exprValue))
				if err != nil {
					return false, err
				}

				if !hasSameType {
					continue
				}

				res, err := filterFieldValueByTypeCode(fieldValue, code)
				if err != nil {
					return false, err
				}
				if res {
					return true, nil
				}

			case string:
				code, err := handlerparams.ParseTypeCode(exprValue)
				if err != nil {
					return false, err
				}
				res, err := filterFieldValueByTypeCode(fieldValue, code)
				if err != nil {
					return false, err
				}
				if res {
					return true, nil
				}
			case int32:
				code, err := handlerparams.NewTypeCode(exprValue)
				if err != nil {
					return false, err
				}

				if !hasSameType {
					continue
				}

				res, err := filterFieldValueByTypeCode(fieldValue, code)
				if err != nil {
					return false, err
				}
				if res {
					return true, nil
				}
			default:
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					fmt.Sprintf(`Invalid numerical type code: %s`, exprValue),
					"$type",
				)
			}
		}
		return false, nil

	case float64:
		if math.IsNaN(exprValue) || math.IsInf(exprValue, 0) {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				`Invalid numerical type code: `+strings.Trim(strings.ToLower(fmt.Sprintf("%v", exprValue)), "+"),
				"$type",
			)
		}
		if exprValue != math.Trunc(exprValue) {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf(`Invalid numerical type code: %v`, exprValue),
				"$type",
			)
		}

		code, err := handlerparams.NewTypeCode(int32(exprValue))
		if err != nil {
			return false, err
		}

		return filterFieldValueByTypeCode(fieldValue, code)

	case string:
		code, err := handlerparams.ParseTypeCode(exprValue)
		if err != nil {
			return false, err
		}

		return filterFieldValueByTypeCode(fieldValue, code)

	case int32:
		code, err := handlerparams.NewTypeCode(exprValue)
		if err != nil {
			return false, err
		}

		return filterFieldValueByTypeCode(fieldValue, code)

	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf(`Invalid numerical type code: %v`, exprValue),
			"$type",
		)
	}
}

// filterFieldValueByTypeCode filters fieldValue by given type code.
func filterFieldValueByTypeCode(fieldValue any, code handlerparams.TypeCode) (bool, error) {
	// check types.Array elements for match to given code.
	if array, ok := fieldValue.(*types.Array); ok && code != handlerparams.TypeCodeArray {
		for i := 0; i < array.Len(); i++ {
			value, _ := array.Get(i)

			// Skip embedded arrays.
			if _, ok := value.(*types.Array); ok {
				continue
			}

			res, err := filterFieldValueByTypeCode(value, code)
			if err != nil {
				return false, err
			}

			if res {
				return true, nil
			}
		}
	}

	switch code {
	case handlerparams.TypeCodeArray:
		if _, ok := fieldValue.(*types.Array); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeObject:
		if _, ok := fieldValue.(*types.Document); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeDouble:
		if _, ok := fieldValue.(float64); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeString:
		if _, ok := fieldValue.(string); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeBinData:
		if _, ok := fieldValue.(types.Binary); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeObjectID:
		if _, ok := fieldValue.(types.ObjectID); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeBool:
		if _, ok := fieldValue.(bool); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeDate:
		if _, ok := fieldValue.(time.Time); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeNull:
		if _, ok := fieldValue.(types.NullType); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeRegex:
		if _, ok := fieldValue.(types.Regex); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeInt:
		if _, ok := fieldValue.(int32); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeTimestamp:
		if _, ok := fieldValue.(types.Timestamp); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeLong:
		if _, ok := fieldValue.(int64); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeNumber:
		// TypeCodeNumber should match int32, int64, float64, and Decimal128 types
		switch fieldValue.(type) {
		case float64, int32, int64, types.Decimal128:
			return true, nil
		default:
			return false, nil
		}
	case handlerparams.TypeCodeDecimal:
		if _, ok := fieldValue.(types.Decimal128); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeMinKey:
		if _, ok := fieldValue.(types.MinKeyType); !ok {
			return false, nil
		}
	case handlerparams.TypeCodeMaxKey:
		if _, ok := fieldValue.(types.MaxKeyType); !ok {
			return false, nil
		}
	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf(`Unknown type name alias: %s`, code.String()),
			"$type",
		)
	}

	return true, nil
}

// filterFieldExprElemMatch handles {field: {$elemMatch: value}}.
// Returns false if doc value is not an array.
//
// Three condition forms are supported:
//   - Pure operator conditions (all keys start with "$", no logical ops, e.g. {$gte: 80, $lt: 90}):
//     iterates each array element and checks all conditions against that single element.
//     This correctly enforces that ALL conditions must be satisfied by the SAME element.
//   - Logical operator conditions ($and, $or, $nor): iterates array elements and calls
//     FilterDocument on each embedded document element.
//   - Field conditions (at least one key without "$", e.g. {a: 1, b: 2} or {score: {$gt: 5}}):
//     iterates array elements and calls FilterDocument on each embedded document element.
func filterFieldExprElemMatch(doc *types.Document, filterKey, filterSuffix string, exprValue any) (bool, error) {
	expr, ok := exprValue.(*types.Document)
	if !ok {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$elemMatch needs an Object",
			"$elemMatch",
		)
	}

	// Classify the expression type by scanning its keys.
	isPureOperator := true // all keys start with "$"
	hasLogical := false    // contains $and, $or, or $nor

	for _, key := range expr.Keys() {
		if slices.Contains([]string{"$text", "$where"}, key) {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("%s can only be applied to the top-level document", key),
				"$elemMatch",
			)
		}

		if slices.Contains([]string{"$and", "$or", "$nor"}, key) {
			hasLogical = true
		}

		if !strings.HasPrefix(key, "$") {
			isPureOperator = false
		}
	}

	value, err := doc.Get(filterSuffix)
	if err != nil {
		return false, nil
	}

	arr, ok := value.(*types.Array)
	if !ok {
		return false, nil
	}

	iter := arr.Iterator()
	defer iter.Close()

	for {
		_, elem, iterErr := iter.Next()
		if errors.Is(iterErr, iterator.ErrIteratorDone) {
			break
		}

		if iterErr != nil {
			return false, lazyerrors.Error(iterErr)
		}

		var matches bool

		if isPureOperator && !hasLogical {
			// Pure operator conditions (e.g. {$gte: 80, $lt: 90}): check all conditions
			// against each individual element. This ensures multi-condition $elemMatch
			// correctly requires ALL conditions to be satisfied by the SAME element,
			// rather than allowing different elements to satisfy different conditions.
			tempDoc := must.NotFail(types.NewDocument(filterSuffix, elem))
			matches, err = filterFieldExpr(tempDoc, filterKey, filterSuffix, expr)
		} else {
			// Logical operators ($and/$or/$nor) or field conditions: the element must be
			// a document and satisfy the expression as a document filter.
			elemDoc, ok := elem.(*types.Document)
			if !ok {
				continue
			}
			matches, err = FilterDocument(elemDoc, expr)
		}

		if err != nil {
			return false, err
		}

		if matches {
			return true, nil
		}
	}

	return false, nil
}

// filterTextSearch implements the $text query operator.
// It parses the $search terms and checks if any string field in the document contains the terms.
// $caseSensitive and $diacriticSensitive flags are respected.
// $language is accepted but not used (language-specific stemming is not implemented).
func filterTextSearch(doc *types.Document, textQuery *types.Document) (bool, error) {
	searchVal, err := textQuery.Get("$search")
	if err != nil {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			`$text requires { $search: <string> }`,
			"$text",
		)
	}

	searchStr, ok := searchVal.(string)
	if !ok {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			`$search requires a string`,
			"$text",
		)
	}

	caseSensitive := false
	if v, _ := textQuery.Get("$caseSensitive"); v != nil {
		if b, ok := v.(bool); ok {
			caseSensitive = b
		}
	}

	diacriticSensitive := false
	if v, _ := textQuery.Get("$diacriticSensitive"); v != nil {
		if b, ok := v.(bool); ok {
			diacriticSensitive = b
		}
	}

	// $language is accepted but stemming is not implemented.

	// Split search string into terms (quoted phrases are treated as single terms).
	terms := parseTextSearchTerms(searchStr)
	if len(terms) == 0 {
		return false, nil
	}

	// Search all string fields (recursively) for any of the terms.
	return docMatchesTextTerms(doc, terms, caseSensitive, diacriticSensitive), nil
}

// parseTextSearchTerms splits a $search string into individual search terms.
// Terms prefixed with '-' are exclusion terms (the document must NOT contain them).
// Quoted phrases are treated as a single term.
// Returns an empty slice if the search string is empty or all terms are negated.
func parseTextSearchTerms(search string) []textTerm {
	var terms []textTerm
	r := []rune(search)
	i := 0

	for i < len(r) {
		// Skip spaces.
		for i < len(r) && r[i] == ' ' {
			i++
		}
		if i >= len(r) {
			break
		}

		negated := false
		if r[i] == '-' {
			negated = true
			i++
		}

		var word []rune

		if i < len(r) && r[i] == '"' {
			// Quoted phrase.
			i++ // skip opening quote
			for i < len(r) && r[i] != '"' {
				word = append(word, r[i])
				i++
			}
			if i < len(r) {
				i++ // skip closing quote
			}
		} else {
			// Plain word.
			for i < len(r) && r[i] != ' ' {
				word = append(word, r[i])
				i++
			}
		}

		if len(word) > 0 {
			terms = append(terms, textTerm{word: string(word), negated: negated})
		}
	}

	return terms
}

// textTerm represents a single search term (may be negated).
type textTerm struct {
	word    string
	negated bool
}

// docMatchesTextTerms returns true if the document's string fields satisfy the text search terms.
// MongoDB $text semantics: at least one positive term must match (OR logic),
// and no negated term may match (AND NOT logic).
func docMatchesTextTerms(doc *types.Document, terms []textTerm, caseSensitive, diacriticSensitive bool) bool {
	// Collect all string values from the document recursively.
	var allStrings []string
	collectStrings(doc, &allStrings)

	anyPositiveMatch := false
	hasPositiveTerm := false

	for _, term := range terms {
		word := term.word
		found := false

		for _, s := range allStrings {
			if textContainsWord(s, word, caseSensitive, diacriticSensitive) {
				found = true
				break
			}
		}

		if term.negated {
			// A negated term must not appear in the document.
			if found {
				return false
			}
		} else {
			hasPositiveTerm = true
			if found {
				anyPositiveMatch = true
			}
		}
	}

	// If there are no positive terms (only negations), any doc that passes negation checks matches.
	if !hasPositiveTerm {
		return true
	}

	return anyPositiveMatch
}

// collectStrings appends all string values from a document (recursively) to the slice.
func collectStrings(doc *types.Document, out *[]string) {
	for _, key := range doc.Keys() {
		val := must.NotFail(doc.Get(key))
		appendStringValue(val, out)
	}
}

// appendStringValue recursively collects string values from a value.
func appendStringValue(val any, out *[]string) {
	switch v := val.(type) {
	case string:
		*out = append(*out, v)
	case *types.Document:
		collectStrings(v, out)
	case *types.Array:
		for i := 0; i < v.Len(); i++ {
			elem := must.NotFail(v.Get(i))
			appendStringValue(elem, out)
		}
	}
}

// stripDiacritics removes diacritic marks from a string using Unicode normalization (NFD + strip Mn).
func stripDiacritics(s string) string {
	t := transform.Chain(norm.NFD, transform.RemoveFunc(func(r rune) bool {
		return unicode.Is(unicode.Mn, r)
	}), norm.NFC)
	result, _, err := transform.String(t, s)
	if err != nil {
		return s
	}
	return result
}

// textContainsWord returns true if the string s contains the word (or phrase) as a match.
// Phrases (terms with spaces) use substring matching; single words use word-boundary matching.
// If caseSensitive is false, the comparison is case-insensitive.
// If diacriticSensitive is false, diacritics are stripped before comparison.
func textContainsWord(s, word string, caseSensitive, diacriticSensitive bool) bool {
	if !diacriticSensitive {
		s = stripDiacritics(s)
		word = stripDiacritics(word)
	}

	if !caseSensitive {
		s = strings.ToLower(s)
		word = strings.ToLower(word)
	}

	// If the term contains spaces, it's a quoted phrase  -- use substring matching.
	if strings.ContainsRune(word, ' ') {
		return strings.Contains(s, word)
	}

	// Split s into words separated by whitespace and punctuation.
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !isWordChar(r)
	})

	for _, w := range words {
		if w == word {
			return true
		}
	}

	return false
}

// isWordChar returns true for characters that are part of a word (letters, digits, underscore).
func isWordChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' ||
		r > 127 // include unicode characters as word chars
}
