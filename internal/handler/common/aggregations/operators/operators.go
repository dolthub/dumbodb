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
		return newOperator(args...)
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

// wholeArgumentOperators take a single argument as written: an array is
// passed through whole, not as a list of arguments, so $literal keeps it and
// the object-argument operators can reject it.
var wholeArgumentOperators = map[string]struct{}{
	"$dateAdd":        {},
	"$dateFromParts":  {},
	"$dateFromString": {},
	"$dateSubtract":   {},
	"$dateToParts":    {},
	"$dateToString":   {},
	"$dayOfMonth":     {},
	"$dayOfWeek":      {},
	"$dayOfYear":      {},
	"$hour":           {},
	"$isoDayOfWeek":   {},
	"$isoWeek":        {},
	"$isoWeekYear":    {},
	"$literal":        {},
	"$millisecond":    {},
	"$minute":         {},
	"$month":          {},
	"$replaceAll":     {},
	"$replaceOne":     {},
	"$second":         {},
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
	"$range":            newRange,
	"$reduce":           newReduce,
	"$regexFind":        newRegexFind,
	"$regexMatch":       newRegexMatch,
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
	"$toBool":           newToBool,
	"$toDate":           newToDate,
	"$toDecimal":        newToDecimal,
	"$toDouble":         newToDouble,
	"$toInt":            newToInt,
	"$toLong":           newToLong,
	"$toLower":          newToLower,
	"$toString":         newToString,
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
	"$binarySize":     {},
	"$covariancePop":  {},
	"$covarianceSamp": {},
	"$denseRank":      {},
	"$derivative":     {},
	"$documentNumber": {},
	"$expMovingAvg":   {},
	"$function":       {},
	"$integral":       {},
	"$linearFill":     {},
	"$locf":           {},
	"$meta":           {},
	"$rand":           {},
	"$rank":           {},
	"$regexFindAll":   {},
	"$sampleRate":     {},
	"$shift":          {},
	"$sortArray":      {},
	"$toObjectId":     {},
	// please keep sorted alphabetically
}
