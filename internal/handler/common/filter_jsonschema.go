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

package common

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// filterJSONSchema implements the $jsonSchema query operator.
// It validates the given document against the provided JSON Schema.
func filterJSONSchema(doc *types.Document, schema *types.Document) (bool, error) {
	return validateJSONSchemaValue(doc, schema, true)
}

// validateJSONSchemaValue recursively validates a value against a JSON Schema document.
func validateJSONSchemaValue(value any, schema *types.Document, isDocument bool) (bool, error) {
	if dupKey, ok := schema.FindDuplicateKey(); ok {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("Duplicate $jsonSchema keyword: %s", dupKey),
			"$jsonSchema",
		)
	}

	for _, key := range schema.Keys() {
		schemaVal := must.NotFail(schema.Get(key))

		switch key {
		case "title", "description":
			// metadata keywords  -- always pass

		case "bsonType":
			ok, err := validateBSONTypeKeyword(value, schemaVal)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}

		case "type":
			ok, err := validateJSONTypeKeyword(value, schemaVal)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}

		case "required":
			arr, ok := schemaVal.(*types.Array)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch,
					fmt.Sprintf("$jsonSchema keyword 'required' must be an array, but found an element of type %s", handlerparams.AliasFromType(schemaVal)),
					"$jsonSchema",
				)
			}

			doc, ok := value.(*types.Document)
			if !ok {
				return false, nil
			}

			for i := 0; i < arr.Len(); i++ {
				field, ok := must.NotFail(arr.Get(i)).(string)
				if !ok {
					return false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						"$jsonSchema keyword 'required' must be an array of strings",
						"$jsonSchema",
					)
				}

				if _, err := doc.Get(field); err != nil {
					return false, nil
				}
			}

		case "properties":
			propsDoc, ok := schemaVal.(*types.Document)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$jsonSchema keyword 'properties' must be an object",
					"$jsonSchema",
				)
			}

			doc, ok := value.(*types.Document)
			if !ok {
				break
			}

			for _, propKey := range propsDoc.Keys() {
				propSchema, ok := must.NotFail(propsDoc.Get(propKey)).(*types.Document)
				if !ok {
					return false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						fmt.Sprintf("$jsonSchema keyword 'properties' schema for %q must be an object", propKey),
						"$jsonSchema",
					)
				}

				fieldVal, err := doc.Get(propKey)
				if err != nil {
					// Field absent  -- properties only validates present fields.
					continue
				}

				ok, err = validateJSONSchemaValue(fieldVal, propSchema, false)
				if err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
			}

		case "enum":
			arr, ok := schemaVal.(*types.Array)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$jsonSchema keyword 'enum' must be an array",
					"$jsonSchema",
				)
			}

			matched := false
			for i := 0; i < arr.Len(); i++ {
				enumVal := must.NotFail(arr.Get(i))
				if types.Compare(value, enumVal) == types.Equal {
					matched = true
					break
				}
			}
			if !matched {
				return false, nil
			}

		case "minimum":
			ok, err := checkNumericRange("minimum", value, schemaVal)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}

		case "maximum":
			ok, err := checkNumericRange("maximum", value, schemaVal)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}

		case "exclusiveMinimum":
			if boolVal, isBool := schemaVal.(bool); isBool {
				// JSON Schema draft-04: boolean modifier for 'minimum'.
				// exclusiveMinimum: true means the 'minimum' boundary is exclusive (value > minimum).
				if boolVal {
					minVal, getErr := schema.Get("minimum")
					if getErr == nil {
						ok, err := checkNumericRange("exclusiveMinimum", value, minVal)
						if err != nil {
							return false, err
						}
						if !ok {
							return false, nil
						}
					}
				}
				// exclusiveMinimum: false means inclusive, handled by 'minimum' case.
			} else {
				// JSON Schema draft-07: numeric exclusive minimum.
				ok, err := checkNumericRange("exclusiveMinimum", value, schemaVal)
				if err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
			}

		case "exclusiveMaximum":
			if boolVal, isBool := schemaVal.(bool); isBool {
				// JSON Schema draft-04: boolean modifier for 'maximum'.
				// exclusiveMaximum: true means the 'maximum' boundary is exclusive (value < maximum).
				if boolVal {
					maxVal, getErr := schema.Get("maximum")
					if getErr == nil {
						ok, err := checkNumericRange("exclusiveMaximum", value, maxVal)
						if err != nil {
							return false, err
						}
						if !ok {
							return false, nil
						}
					}
				}
				// exclusiveMaximum: false means inclusive, handled by 'maximum' case.
			} else {
				// JSON Schema draft-07: numeric exclusive maximum.
				ok, err := checkNumericRange("exclusiveMaximum", value, schemaVal)
				if err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
			}

		case "multipleOf":
			ok, err := checkMultipleOf(value, schemaVal)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}

		case "minLength":
			str, ok := value.(string)
			if !ok {
				break
			}

			limit, err := schemaIntegerValue("minLength", schemaVal)
			if err != nil {
				return false, err
			}

			if int64(len([]rune(str))) < limit {
				return false, nil
			}

		case "maxLength":
			str, ok := value.(string)
			if !ok {
				break
			}

			limit, err := schemaIntegerValue("maxLength", schemaVal)
			if err != nil {
				return false, err
			}

			if int64(len([]rune(str))) > limit {
				return false, nil
			}

		case "pattern":
			str, ok := value.(string)
			if !ok {
				break
			}

			patternStr, ok := schemaVal.(string)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$jsonSchema keyword 'pattern' must be a string",
					"$jsonSchema",
				)
			}

			re, err := regexp.Compile(patternStr)
			if err != nil {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					fmt.Sprintf("$jsonSchema keyword 'pattern' is not a valid regex: %v", err),
					"$jsonSchema",
				)
			}

			if !re.MatchString(str) {
				return false, nil
			}

		case "minItems":
			arr, ok := value.(*types.Array)
			if !ok {
				break
			}

			limit, err := schemaIntegerValue("minItems", schemaVal)
			if err != nil {
				return false, err
			}

			if int64(arr.Len()) < limit {
				return false, nil
			}

		case "maxItems":
			arr, ok := value.(*types.Array)
			if !ok {
				break
			}

			limit, err := schemaIntegerValue("maxItems", schemaVal)
			if err != nil {
				return false, err
			}

			if int64(arr.Len()) > limit {
				return false, nil
			}

		case "uniqueItems":
			b, ok := schemaVal.(bool)
			if !ok || !b {
				break
			}

			arr, ok := value.(*types.Array)
			if !ok {
				break
			}

			if !arrayItemsUnique(arr) {
				return false, nil
			}

		case "items":
			arr, ok := value.(*types.Array)
			if !ok {
				break
			}

			switch items := schemaVal.(type) {
			case *types.Document:
				// Single schema applied to all array elements.
				iter := arr.Iterator()
				defer iter.Close()

				for {
					_, elem, err := iter.Next()
					if errors.Is(err, iterator.ErrIteratorDone) {
						break
					}
					if err != nil {
						return false, err
					}

					ok, err := validateJSONSchemaValue(elem, items, false)
					if err != nil {
						return false, err
					}
					if !ok {
						return false, nil
					}
				}

			case *types.Array:
				// Tuple validation: positional schemas.
				for i := 0; i < items.Len() && i < arr.Len(); i++ {
					itemSchema, ok := must.NotFail(items.Get(i)).(*types.Document)
					if !ok {
						continue
					}

					elem := must.NotFail(arr.Get(i))
					ok, err := validateJSONSchemaValue(elem, itemSchema, false)
					if err != nil {
						return false, err
					}
					if !ok {
						return false, nil
					}
				}
			}

		case "additionalProperties":
			b, ok := schemaVal.(bool)
			if !ok || b {
				break
			}

			doc, ok := value.(*types.Document)
			if !ok {
				break
			}

			// Build set of defined property names.
			definedProps := map[string]bool{}
			if propsVal, err := schema.Get("properties"); err == nil {
				if pd, ok := propsVal.(*types.Document); ok {
					for _, k := range pd.Keys() {
						definedProps[k] = true
					}
				}
			}

			for _, k := range doc.Keys() {
				if !definedProps[k] {
					return false, nil
				}
			}

		case "allOf":
			arr, ok := schemaVal.(*types.Array)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$jsonSchema keyword 'allOf' must be an array",
					"$jsonSchema",
				)
			}

			for i := 0; i < arr.Len(); i++ {
				subSchema, ok := must.NotFail(arr.Get(i)).(*types.Document)
				if !ok {
					return false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						"$jsonSchema keyword 'allOf' must be an array of schema objects",
						"$jsonSchema",
					)
				}

				ok, err := validateJSONSchemaValue(value, subSchema, isDocument)
				if err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
			}

		case "anyOf":
			arr, ok := schemaVal.(*types.Array)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$jsonSchema keyword 'anyOf' must be an array",
					"$jsonSchema",
				)
			}

			anyMatched := false
			for i := 0; i < arr.Len(); i++ {
				subSchema, ok := must.NotFail(arr.Get(i)).(*types.Document)
				if !ok {
					return false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						"$jsonSchema keyword 'anyOf' must be an array of schema objects",
						"$jsonSchema",
					)
				}

				ok, err := validateJSONSchemaValue(value, subSchema, isDocument)
				if err != nil {
					return false, err
				}
				if ok {
					anyMatched = true
					break
				}
			}

			if !anyMatched {
				return false, nil
			}

		case "oneOf":
			arr, ok := schemaVal.(*types.Array)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$jsonSchema keyword 'oneOf' must be an array",
					"$jsonSchema",
				)
			}

			matchCount := 0
			for i := 0; i < arr.Len(); i++ {
				subSchema, ok := must.NotFail(arr.Get(i)).(*types.Document)
				if !ok {
					return false, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						"$jsonSchema keyword 'oneOf' must be an array of schema objects",
						"$jsonSchema",
					)
				}

				ok, err := validateJSONSchemaValue(value, subSchema, isDocument)
				if err != nil {
					return false, err
				}
				if ok {
					matchCount++
				}
			}

			if matchCount != 1 {
				return false, nil
			}

		case "not":
			subSchema, ok := schemaVal.(*types.Document)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$jsonSchema keyword 'not' must be a schema object",
					"$jsonSchema",
				)
			}

			ok, err := validateJSONSchemaValue(value, subSchema, isDocument)
			if err != nil {
				return false, err
			}
			if ok {
				return false, nil
			}

		default:
			// Unknown keywords are ignored per JSON Schema spec.
		}
	}

	return true, nil
}

// validateBSONTypeKeyword checks if value matches the bsonType constraint.
// schemaVal may be a string or an array of strings.
func validateBSONTypeKeyword(value any, schemaVal any) (bool, error) {
	var typeNames []string

	switch sv := schemaVal.(type) {
	case string:
		typeNames = []string{sv}
	case *types.Array:
		for i := 0; i < sv.Len(); i++ {
			typeName, ok := must.NotFail(sv.Get(i)).(string)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$jsonSchema keyword 'bsonType' must be a string or an array of strings",
					"$jsonSchema",
				)
			}
			typeNames = append(typeNames, typeName)
		}
	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$jsonSchema keyword 'bsonType' must be a string or an array of strings",
			"$jsonSchema",
		)
	}

	for _, typeName := range typeNames {
		if valueMatchesBSONType(value, strings.ToLower(typeName)) {
			return true, nil
		}
	}

	return false, nil
}

// valueMatchesBSONType returns true if value has the given BSON type name.
func valueMatchesBSONType(value any, typeName string) bool {
	switch typeName {
	case "double":
		_, ok := value.(float64)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "object":
		_, ok := value.(*types.Document)
		return ok
	case "array":
		_, ok := value.(*types.Array)
		return ok
	case "bindata":
		_, ok := value.(types.Binary)
		return ok
	case "objectid":
		_, ok := value.(types.ObjectID)
		return ok
	case "bool", "boolean":
		_, ok := value.(bool)
		return ok
	case "date":
		_, ok := value.(time.Time)
		return ok
	case "null":
		_, ok := value.(types.NullType)
		return ok
	case "regex":
		_, ok := value.(types.Regex)
		return ok
	case "int":
		_, ok := value.(int32)
		return ok
	case "timestamp":
		_, ok := value.(types.Timestamp)
		return ok
	case "long":
		_, ok := value.(int64)
		return ok
	case "decimal":
		_, ok := value.(types.Decimal128)
		return ok
	case "number":
		switch value.(type) {
		case float64, int32, int64:
			return true
		}
		return false
	default:
		return false
	}
}

// validateJSONTypeKeyword checks if value matches JSON Schema type names.
func validateJSONTypeKeyword(value any, schemaVal any) (bool, error) {
	var typeNames []string

	switch sv := schemaVal.(type) {
	case string:
		typeNames = []string{sv}
	case *types.Array:
		for i := 0; i < sv.Len(); i++ {
			typeName, ok := must.NotFail(sv.Get(i)).(string)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$jsonSchema keyword 'type' must be a string or an array of strings",
					"$jsonSchema",
				)
			}
			typeNames = append(typeNames, typeName)
		}
	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$jsonSchema keyword 'type' must be a string or an array of strings",
			"$jsonSchema",
		)
	}

	for _, typeName := range typeNames {
		if valueMatchesJSONType(value, typeName) {
			return true, nil
		}
	}

	return false, nil
}

// valueMatchesJSONType checks if value matches a JSON Schema type name.
func valueMatchesJSONType(value any, typeName string) bool {
	switch typeName {
	case "number":
		switch value.(type) {
		case float64, int32, int64:
			return true
		}
		return false
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(*types.Document)
		return ok
	case "array":
		_, ok := value.(*types.Array)
		return ok
	case "null":
		_, ok := value.(types.NullType)
		return ok
	case "integer":
		switch v := value.(type) {
		case int32, int64:
			return true
		case float64:
			return v == math.Trunc(v) && !math.IsInf(v, 0) && !math.IsNaN(v)
		}
		return false
	default:
		return false
	}
}

// checkNumericRange validates minimum/maximum/exclusiveMinimum/exclusiveMaximum constraints.
func checkNumericRange(keyword string, value, limitVal any) (bool, error) {
	var val float64

	switch v := value.(type) {
	case float64:
		val = v
	case int32:
		val = float64(v)
	case int64:
		val = float64(v)
	default:
		// Non-numeric values pass numeric range constraints.
		return true, nil
	}

	var lim float64

	switch l := limitVal.(type) {
	case float64:
		lim = l
	case int32:
		lim = float64(l)
	case int64:
		lim = float64(l)
	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$jsonSchema keyword '%s' must be a number", keyword),
			"$jsonSchema",
		)
	}

	switch keyword {
	case "minimum":
		if val < lim {
			return false, nil
		}
	case "maximum":
		if val > lim {
			return false, nil
		}
	case "exclusiveMinimum":
		if val <= lim {
			return false, nil
		}
	case "exclusiveMaximum":
		if val >= lim {
			return false, nil
		}
	}

	return true, nil
}

// schemaIntegerValue extracts a non-negative integer from a schema keyword value.
func schemaIntegerValue(keyword string, schemaVal any) (int64, error) {
	switch v := schemaVal.(type) {
	case int32:
		if v < 0 {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("$jsonSchema keyword '%s' must be a non-negative integer", keyword),
				"$jsonSchema",
			)
		}
		return int64(v), nil
	case int64:
		if v < 0 {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("$jsonSchema keyword '%s' must be a non-negative integer", keyword),
				"$jsonSchema",
			)
		}
		return v, nil
	case float64:
		if v != math.Trunc(v) || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("$jsonSchema keyword '%s' must be a non-negative integer", keyword),
				"$jsonSchema",
			)
		}
		return int64(v), nil
	default:
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$jsonSchema keyword '%s' must be a non-negative integer", keyword),
			"$jsonSchema",
		)
	}
}

// arrayItemsUnique returns true if all items in the array are distinct.
func arrayItemsUnique(arr *types.Array) bool {
	for i := 0; i < arr.Len(); i++ {
		for j := i + 1; j < arr.Len(); j++ {
			vi := must.NotFail(arr.Get(i))
			vj := must.NotFail(arr.Get(j))
			if types.Compare(vi, vj) == types.Equal {
				return false
			}
		}
	}
	return true
}

// checkMultipleOf validates the multipleOf constraint.
// The numeric value must be evenly divisible by the given modulus.
// Non-numeric values pass the constraint.
func checkMultipleOf(value, modVal any) (bool, error) {
	var val float64
	switch v := value.(type) {
	case float64:
		val = v
	case int32:
		val = float64(v)
	case int64:
		val = float64(v)
	default:
		return true, nil
	}

	var mod float64
	switch m := modVal.(type) {
	case float64:
		mod = m
	case int32:
		mod = float64(m)
	case int64:
		mod = float64(m)
	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$jsonSchema keyword 'multipleOf' must be a number",
			"$jsonSchema",
		)
	}

	if mod <= 0 {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$jsonSchema keyword 'multipleOf' must be a positive number",
			"$jsonSchema",
		)
	}

	rem := math.Remainder(val, mod)
	return math.Abs(rem) <= 1e-9, nil
}
