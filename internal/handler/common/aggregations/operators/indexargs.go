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
	"fmt"
	"math"
	"strconv"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// doubleString formats a number as mongod's messages print coerceToDouble.
func doubleString(v any) string {
	return strconv.FormatFloat(toFloat64(v), 'g', 6, 64)
}

// coerceToLong is Value::coerceToLong for a number: doubles truncate,
// decimals round half to even, and values outside the long range fail.
func coerceToLong(v any) (int64, error) {
	switch n := v.(type) {
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		if math.IsNaN(n) || n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, handlererrors.NewCommandErrorMsg(handlererrors.ErrCoerceLongOutOfRange,
				fmt.Sprintf("Can't coerce out of range value %s to long", doubleString(n)))
		}
		return int64(n), nil
	case types.Decimal128:
		rounded := new(apd.Decimal)
		x := aggregations.DecimalToAPD(n)
		if x.Form == apd.Finite {
			_, _ = apd.BaseContext.WithPrecision(1000).RoundToIntegralValue(rounded, x)
			if i, err := rounded.Int64(); err == nil {
				return i, nil
			}
		}
		return 0, handlererrors.NewCommandErrorMsg(handlererrors.ErrCoerceLongOutOfRange,
			fmt.Sprintf("Can't coerce out of range value %s to long", doubleString(n)))
	default:
		return 0, handlererrors.NewCommandErrorMsg(handlererrors.ErrCannotConvertToLong,
			fmt.Sprintf("can't convert from BSON type %s to long", handlerparams.AliasFromType(v)))
	}
}

// requireIndex is mongod's uassertIfNotIntegralAndNonNegative.
func requireIndex(v any, opName, argName string) (int, error) {
	i, ok := integralInt(v)
	if !ok {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexNotIntegral,
			fmt.Sprintf("%srequires an integral %s, found a value of type: %s, with value: %s",
				opName, argName, handlerparams.AliasFromType(v), scalarString(v)), opName)
	}
	if i < 0 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexNegative,
			fmt.Sprintf("%s requires a nonnegative %s, found: %d", opName, argName, i), opName)
	}
	return i, nil
}

// substrBytes is ExpressionSubstrBytes::evaluate after the string is coerced.
func substrBytes(opName, s string, lowerV, lengthV any) (any, error) {
	if !isNumber(lowerV) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrStartNotNumeric,
			fmt.Sprintf("%s:  starting index must be a numeric type (is BSON type %s)", opName, handlerparams.AliasFromType(lowerV)), opName)
	}
	if !isNumber(lengthV) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrLengthNotNumeric,
			fmt.Sprintf("%s:  length must be a numeric type (is BSON type %s)", opName, handlerparams.AliasFromType(lengthV)), opName)
	}
	lower, err := coerceToLong(lowerV)
	if err != nil {
		return nil, err
	}
	if lower < 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrStartNegative,
			fmt.Sprintf("%s:  starting index must be non-negative (got: %d)", opName, lower), opName)
	}
	signedLength, err := coerceToLong(lengthV)
	if err != nil {
		return nil, err
	}
	length := uint64(len(s))
	if signedLength >= 0 {
		length = uint64(signedLength)
	}

	n := uint64(len(s))
	start := uint64(lower)
	if start < n && isUTF8Continuation(s[start]) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrStartContinuation,
			opName+":  Invalid range, starting index is a UTF-8 continuation byte.", opName)
	}
	end := n
	if length < n-min(start, n) {
		end = start + length
	}
	if end < n && isUTF8Continuation(s[end]) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrEndMidCharacter,
			opName+":  Invalid range, ending index is in the middle of a UTF-8 character.", opName)
	}
	if start >= n {
		return "", nil
	}
	return s[start:end], nil
}

func isUTF8Continuation(b byte) bool {
	return b&0xC0 == 0x80
}

// utf8CodePointLength is str::getCodePointLength: the length a lead byte
// announces, which may exceed 4 for invalid input.
func utf8CodePointLength(b byte) int {
	switch {
	case b < 0x80:
		return 1
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	default:
		return 5
	}
}
