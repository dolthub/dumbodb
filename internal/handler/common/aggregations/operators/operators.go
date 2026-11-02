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

// Package operators provides aggregation operators.
// Operators are used in aggregation stages to filter and model data.
// This package contains all operators apart from the accumulation operators,
// which are stored and described in accumulators package.
//
// Accumulators that can be used outside of accumulation with different behaviour (like `$sum`),
// should be stored in both operators and accumulators packages.
package operators

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// newOperatorFunc is a type for a function that creates a standard aggregation operator.
//
// By standard aggregation operator we mean any operator that is not accumulator.
// While accumulators perform operations on multiple documents
// (for example `$count` can count documents in each `$group` group),
// standard operators perform operations on a single document.
// It takes the arguments extracted from the document, and not the
// whole array/document.
type newOperatorFunc func(args ...any) (Operator, error)

// Operator is a common interface for standard aggregation operators.
type Operator interface {
	// Process document and returns the result of applying operator.
	Process(in *types.Document) (any, error)
}

// IsOperator returns true if provided document should be
// treated as operator document.
func IsOperator(doc *types.Document) bool {
	iter := doc.Iterator()
	defer iter.Close()

	for {
		key, _, err := iter.Next()
		if err != nil {
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			return false
		}

		if strings.HasPrefix(key, "$") {
			return true
		}
	}

	return false
}

// NewOperator returns operator from provided document.
// The document should look like: `{<$operator>: <operator-value>}`.
//
// Before calling NewOperator on document it's recommended to validate
// document before by using IsOperator on it.
func NewOperator(doc *types.Document) (Operator, error) {
	if doc.Len() == 0 {
		return nil, lazyerrors.New(
			"The operator field is empty",
		)
	}

	if doc.Len() > 1 {
		return nil, newOperatorError(
			ErrTooManyFields,
			doc.Command(),
			fmt.Sprintf("An object representing an expression must have exactly one field: %s", types.FormatAnyValue(doc)),
		)
	}

	operator := doc.Command()

	newOperator, supported := Operators[operator]
	_, unsupported := unsupportedOperators[operator]

	expr := must.NotFail(doc.Get(operator))

	var args []any

	_, wholeArgument := wholeArgumentOperators[operator]
	if arr, ok := expr.(*types.Array); ok && !wholeArgument {
		iter := arr.Iterator()
		defer iter.Close()

		for {
			_, v, err := iter.Next()

			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			args = append(args, v)
		}
	} else {
		args = append(args, expr)
	}

	switch {
	case supported:
		op, err := newOperator(args...)
		if err != nil || operator == "$literal" {
			return op, err
		}
		if err = parseNested(nestedExpressions(operator, args)); err != nil {
			return nil, AsNestedError(err)
		}
		return op, nil
	case unsupported:
		return nil, newOperatorError(
			ErrNotImplemented,
			operator,
			fmt.Sprintf("The operator %s is not implemented yet", operator),
		)
	default:
		return nil, newOperatorError(
			ErrInvalidExpression,
			operator,
			fmt.Sprintf("Unrecognized expression '%s'", operator),
		)
	}
}

// nonExpressionFields are argument fields that hold data, not expressions.
var nonExpressionFields = map[string]string{
	"$sortArray": "sortBy",
}

// nestedExpressions returns the arguments of operator that are expressions.
func nestedExpressions(operator string, args []any) []any {
	field, ok := nonExpressionFields[operator]
	if !ok || len(args) != 1 {
		return args
	}
	spec, ok := args[0].(*types.Document)
	if !ok {
		return args
	}
	var exprs []any
	for _, key := range spec.Keys() {
		if key != field {
			exprs = append(exprs, must.NotFail(spec.Get(key)))
		}
	}
	return exprs
}

// parseNested builds every operator nested in args, as mongod parses and
// constant-folds a whole expression before evaluating any of it: a branch
// that never runs still reports its parse errors.
func parseNested(args []any) error {
	for _, arg := range args {
		switch v := arg.(type) {
		case *types.Document:
			if IsOperator(v) {
				if _, err := cachedOperator(v); err != nil {
					return err
				}
				continue
			}
			if err := parseNested(v.Values()); err != nil {
				return err
			}
		case *types.Array:
			vals := make([]any, v.Len())
			for i := range vals {
				vals[i] = must.NotFail(v.Get(i))
			}
			if err := parseNested(vals); err != nil {
				return err
			}
		}
	}
	return nil
}

// wholeArgumentOperators take a single argument as written: an array is
// passed through whole, not as a list of arguments, so $literal keeps it and
// the object-argument operators can reject it.
var wholeArgumentOperators = map[string]struct{}{
	"$convert":        {},
	"$dateAdd":        {},
	"$dateDiff":       {},
	"$dateFromParts":  {},
	"$dateFromString": {},
	"$dateSubtract":   {},
	"$dateToParts":    {},
	"$dateToString":   {},
	"$dateTrunc":      {},
	"$dayOfMonth":     {},
	"$dayOfWeek":      {},
	"$dayOfYear":      {},
	"$hour":           {},
	"$isoDayOfWeek":   {},
	"$isoWeek":        {},
	"$isoWeekYear":    {},
	"$literal":        {},
	"$meta":           {},
	"$millisecond":    {},
	"$minute":         {},
	"$month":          {},
	"$rand":           {},
	"$regexFind":      {},
	"$regexFindAll":   {},
	"$regexMatch":     {},
	"$replaceAll":     {},
	"$replaceOne":     {},
	"$second":         {},
	"$sortArray":      {},
	"$week":           {},
	"$year":           {},
}

// Operators maps all standard aggregation operators.
var Operators = map[string]newOperatorFunc{
	// sorted alphabetically
	"$abs":              newAbs,
	"$acos":             newAcos,
	"$acosh":            newAcosh,
	"$add":              newAdd,
	"$allElementsTrue":  newAllElementsTrue,
	"$and":              newAnd,
	"$arrayToObject":    newArrayToObject,
	"$asin":             newAsin,
	"$asinh":            newAsinh,
	"$atan":             newAtan,
	"$atan2":            newAtan2,
	"$atanh":            newAtanh,
	"$avg":              newAvgOp,
	"$anyElementTrue":   newAnyElementTrue,
	"$arrayElemAt":      newArrayElemAt,
	"$binarySize":       newBinarySize,
	"$bsonSize":         newBsonSize,
	"$ceil":             newCeil,
	"$cmp":              newCmp,
	"$concat":           newConcat,
	"$concatArrays":     newConcatArrays,
	"$cond":             newCond,
	"$convert":          newConvert,
	"$cos":              newCos,
	"$cosh":             newCosh,
	"$dateAdd":          newDateArithmetic("$dateAdd"),
	"$dateDiff":         newDateDiff,
	"$dateFromParts":    newDateFromParts,
	"$dateFromString":   newDateFromString,
	"$dateSubtract":     newDateArithmetic("$dateSubtract"),
	"$dateToParts":      newDateToParts,
	"$dateTrunc":        newDateTrunc,
	"$dateToString":     newDateToString,
	"$degreesToRadians": newDegreesToRadians,
	"$dayOfMonth":       newDatePartOp("$dayOfMonth"),
	"$dayOfWeek":        newDatePartOp("$dayOfWeek"),
	"$dayOfYear":        newDatePartOp("$dayOfYear"),
	"$divide":           newDivide,
	"$eq":               newCmpOperator("$eq", compEq),
	"$exp":              newExp,
	"$filter":           newFilter,
	"$first":            newFirstOp,
	"$firstN":           newNOpFunc("$firstN"),
	"$floor":            newFloor,
	"$getField":         newGetField,
	"$gt":               newCmpOperator("$gt", compGt),
	"$gte":              newCmpOperator("$gte", compGte),
	"$hour":             newDatePartOp("$hour"),
	"$ifNull":           newIfNull,
	"$in":               newInArray,
	"$indexOfArray":     newIndexOfArray,
	"$indexOfBytes":     newIndexOfBytes,
	"$indexOfCP":        newIndexOfCP,
	"$isArray":          newIsArray,
	"$isDate":           newIsDate,
	"$isNumber":         newIsNumber,
	"$isObjectId":       newIsObjectId,
	"$isString":         newIsString,
	"$isoDayOfWeek":     newDatePartOp("$isoDayOfWeek"),
	"$isoWeek":          newDatePartOp("$isoWeek"),
	"$isoWeekYear":      newDatePartOp("$isoWeekYear"),
	"$last":             newLastOp,
	"$lastN":            newNOpFunc("$lastN"),
	"$let":              newLet,
	"$literal":          newLiteral,
	"$ln":               newLn,
	"$log":              newLog,
	"$log10":            newLog10,
	"$lt":               newCmpOperator("$lt", compLt),
	"$ltrim":            newLtrim,
	"$lte":              newCmpOperator("$lte", compLte),
	"$map":              newMap,
	"$max":              newMaxOp,
	"$maxN":             newNOpFunc("$maxN"),
	"$median":           newPercentileFunc("$median", true),
	"$mergeObjects":     newMergeObjects,
	"$meta":             newMeta,
	"$millisecond":      newDatePartOp("$millisecond"),
	"$min":              newMinOp,
	"$minN":             newNOpFunc("$minN"),
	"$minute":           newDatePartOp("$minute"),
	"$mod":              newMod,
	"$month":            newDatePartOp("$month"),
	"$multiply":         newMultiply,
	"$ne":               newCmpOperator("$ne", compNe),
	"$not":              newNot,
	"$objectToArray":    newObjectToArray,
	"$or":               newOr,
	"$percentile":       newPercentileFunc("$percentile", false),
	"$pow":              newPow,
	"$radiansToDegrees": newRadiansToDegrees,
	"$rand":             newRand,
	"$range":            newRange,
	"$reduce":           newReduce,
	"$regexFind":        newRegex("$regexFind"),
	"$regexFindAll":     newRegex("$regexFindAll"),
	"$regexMatch":       newRegex("$regexMatch"),
	"$replaceAll":       newReplace("$replaceAll"),
	"$replaceOne":       newReplace("$replaceOne"),
	"$reverseArray":     newReverseArray,
	"$round":            newRound,
	"$rtrim":            newRtrim,
	"$second":           newDatePartOp("$second"),
	"$setField":         newSetField,
	"$setDifference":    newSetDifference,
	"$setEquals":        newSetEquals,
	"$setIntersection":  newSetIntersection,
	"$setIsSubset":      newSetIsSubset,
	"$setUnion":         newSetUnion,
	"$sin":              newSin,
	"$sinh":             newSinh,
	"$size":             newSize,
	"$slice":            newSlice,
	"$sortArray":        newSortArray,
	"$split":            newSplit,
	"$sqrt":             newSqrt,
	"$stdDevPop":        newStdDevPopOp,
	"$stdDevSamp":       newStdDevSampOp,
	"$strcasecmp":       newStrcasecmp,
	"$strLenBytes":      newStrLenBytes,
	"$strLenCP":         newStrLenCP,
	"$substr":           newSubstr,
	"$substrBytes":      newSubstrBytesOp,
	"$substrCP":         newSubstrCP,
	"$subtract":         newSubtract,
	"$sum":              newSum,
	"$switch":           newSwitch,
	"$tan":              newTan,
	"$tanh":             newTanh,
	"$toBool":           newConversionAlias("$toBool", "bool", nil),
	"$toDate":           newConversionAlias("$toDate", "date", nil),
	"$toDecimal":        newConversionAlias("$toDecimal", "decimal", nil),
	"$toDouble":         newConversionAlias("$toDouble", "double", nil),
	"$toInt":            newConversionAlias("$toInt", "int", nil),
	"$toLong":           newConversionAlias("$toLong", "long", nil),
	"$toLower":          newToLower,
	"$toObjectId":       newConversionAlias("$toObjectId", "objectId", nil),
	"$toString":         newConversionAlias("$toString", "string", "auto"),
	"$toUUID":           newConversionAlias("$toUUID", must.NotFail(types.NewDocument("type", "binData", "subtype", int32(4))), "uuid"),
	"$toUpper":          newToUpper,
	"$unsetField":       newUnsetField,
	"$trim":             newTrim,
	"$trunc":            newTrunc,
	"$tsIncrement":      newTsIncrement,
	"$tsSecond":         newTsSecond,
	"$type":             newType,
	"$week":             newDatePartOp("$week"),
	"$year":             newDatePartOp("$year"),
	"$zip":              newZip,
	// please keep sorted alphabetically
}

// unsupportedOperators maps all unsupported yet operators.
var unsupportedOperators = map[string]struct{}{
	// sorted alphabetically
	"$function": {},
	// please keep sorted alphabetically
}
