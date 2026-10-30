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
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cockroachdb/apd/v3"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// BSON type codes as $convert's 'to' names them.
const (
	bsonMinKey     = -1
	bsonMissing    = 0
	bsonDouble     = 1
	bsonString     = 2
	bsonObject     = 3
	bsonArray      = 4
	bsonBinData    = 5
	bsonUndefined  = 6
	bsonObjectID   = 7
	bsonBool       = 8
	bsonDate       = 9
	bsonNull       = 10
	bsonRegex      = 11
	bsonInt        = 16
	bsonTimestamp  = 17
	bsonLong       = 18
	bsonDecimal    = 19
	bsonMaxKey     = 127
	binDataUUID    = 4
	binDataColumn  = 7
	binDataUserMin = 128
)

var bsonTypeNames = map[int]string{
	bsonMinKey: "minKey", bsonMissing: "missing", bsonDouble: "double", bsonString: "string",
	bsonObject: "object", bsonArray: "array", bsonBinData: "binData", bsonUndefined: "undefined",
	bsonObjectID: "objectId", bsonBool: "bool", bsonDate: "date", bsonNull: "null", bsonRegex: "regex",
	12: "dbPointer", 13: "javascript", 14: "symbol", 15: "javascriptWithScope",
	bsonInt: "int", bsonTimestamp: "timestamp", bsonLong: "long", bsonDecimal: "decimal", bsonMaxKey: "maxKey",
}

var bsonTypeCodes = func() map[string]int {
	codes := make(map[string]int, len(bsonTypeNames))
	for code, name := range bsonTypeNames {
		codes[name] = code
	}
	return codes
}()

// binDataFormats are the values of $convert's 'format'.
var binDataFormats = map[string]bool{"auto": true, "base64": true, "base64url": true, "hex": true, "utf8": true, "uuid": true}

// convertOp is ExpressionConvert. The $to* shorthands are $convert with a
// fixed 'to' (and 'format' for $toString and $toUUID) and no onError or
// onNull.
type convertOp struct {
	input, to, format, onError, onNull any
}

func newConvert(args ...any) (Operator, error) {
	spec, ok := args[0].(*types.Document)
	if len(args) != 1 || !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"$convert expects an object of named arguments but found: "+handlerparams.AliasFromType(args[0]), "$convert")
	}

	op := new(convertOp)
	for _, key := range spec.Keys() {
		v := must.NotFail(spec.Get(key))
		switch key {
		case "input":
			op.input = v
		case "to":
			op.to = v
		case "format":
			op.format = v
		case "onError":
			op.onError = v
		case "onNull":
			op.onNull = v
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
				"$convert found an unknown argument: "+key, "$convert")
		}
	}
	if op.input == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"Missing 'input' parameter to $convert", "$convert")
	}
	if op.to == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"Missing 'to' parameter to $convert", "$convert")
	}

	return op.checkConstants()
}

// newConversionAlias is makeConversionAlias.
func newConversionAlias(name string, to, format any) newOperatorFunc {
	return func(args ...any) (Operator, error) {
		if len(args) != 1 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConversionArity,
				fmt.Sprintf("%s requires a single argument, got %d", name, len(args)), name)
		}
		return (&convertOp{input: args[0], to: to, format: format}).checkConstants()
	}
}

// checkConstants evaluates a $convert whose operands are all constants, so its
// errors surface while parsing, as mongod's optimize() does.
func (op *convertOp) checkConstants() (Operator, error) {
	for _, operand := range []any{op.input, op.to, op.format, op.onError, op.onNull} {
		if operand != nil && !isConstantExpression(operand) {
			return op, nil
		}
	}
	if _, err := op.Process(nil); err != nil && !errors.Is(err, aggregations.ErrMissingValue) {
		return nil, err
	}
	return op, nil
}

// Process is ExpressionConvert::evaluate.
func (op *convertOp) Process(doc *types.Document) (any, error) {
	to, toMissing, err := foldConstant(op.to, doc)
	if err != nil {
		return nil, err
	}
	input, inputMissing, err := foldConstant(op.input, doc)
	if err != nil {
		return nil, err
	}
	var format any
	formatMissing := true
	if op.format != nil {
		if format, formatMissing, err = foldConstant(op.format, doc); err != nil {
			return nil, err
		}
	}

	target, subtype, hasTarget, err := parseConvertTarget(to, toMissing)
	if err != nil {
		return nil, err
	}

	if inputMissing || isNullish(input) {
		if op.onNull == nil {
			return types.Null, nil
		}
		return evalOrMissing(op.onNull, doc)
	}
	if !hasTarget {
		return types.Null, nil
	}

	formatName, err := parseBinDataFormat(format, formatMissing)
	if err != nil {
		return nil, err
	}

	res, err := convertValue(input, target, formatName, subtype)
	if err != nil && op.onError != nil && isConversionFailure(err) {
		return evalOrMissing(op.onError, doc)
	}
	return res, err
}

// evalOrMissing evaluates expr, reporting a missing result as ErrMissingValue.
func evalOrMissing(expr any, doc *types.Document) (any, error) {
	v, missing, err := foldConstant(expr, doc)
	if err == nil && missing {
		return nil, aggregations.ErrMissingValue
	}
	return v, err
}

func isConversionFailure(err error) bool {
	var cmdErr *handlererrors.CommandError
	return errors.As(err, &cmdErr) && cmdErr.Code() == handlererrors.ErrConversionFailure
}

// parseConvertTarget is ConvertTargetTypeInfo::parse; ok is false when 'to'
// is nullish.
func parseConvertTarget(to any, missing bool) (target int, subtype any, ok bool, err error) {
	if missing || isNullish(to) {
		return 0, nil, false, nil
	}

	typeValue := to
	typeMissing := false
	subtype = int32(0)
	if spec, isDoc := to.(*types.Document); isDoc {
		typeValue, err = spec.Get("type")
		typeMissing = err != nil
		if s, err := spec.Get("subtype"); err == nil {
			subtype = s
		}
	}

	target, err = computeTargetType(typeValue, typeMissing)
	return target, subtype, err == nil, err
}

// computeTargetType is ExpressionConvert::computeTargetType.
func computeTargetType(v any, missing bool) (int, error) {
	if missing {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"$convert's 'to' argument must be a string or number, but is missing", "$convert")
	}
	if name, ok := v.(string); ok {
		code, known := bsonTypeCodes[name]
		if !known {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
				"Unknown type name: "+name, "$convert")
		}
		return code, nil
	}
	if !isNumber(v) {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"$convert's 'to' argument must be a string or number, but is "+handlerparams.AliasFromType(v), "$convert")
	}
	code, integral := integralInt(v)
	if !integral {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"In $convert, numeric 'to' argument is not an integer", "$convert")
	}
	if _, valid := bsonTypeNames[code]; !valid || code == bsonMissing {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			fmt.Sprintf("In $convert, numeric value for 'to' does not correspond to a BSON type: %d", code), "$convert")
	}
	return code, nil
}

// integralInt returns a number as an int when it is a whole number in int
// range.
func integralInt(v any) (int, bool) {
	switch n := v.(type) {
	case int32:
		return int(n), true
	case int64:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case float64:
		if n != math.Trunc(n) || n < math.MinInt32 || n > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case types.Decimal128:
		i, ok := aggregations.Decimal128ToInt64(n)
		if !ok || i < math.MinInt32 || i > math.MaxInt32 {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

// parseBinDataFormat returns "" when format is nullish.
func parseBinDataFormat(format any, missing bool) (string, error) {
	if missing || isNullish(format) {
		return "", nil
	}
	name, ok := format.(string)
	if !ok {
		return "", handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConvertFormatType,
			fmt.Sprintf("$convert requires that 'format' be a string, found: %s with value %s",
				handlerparams.AliasFromType(format), types.FormatAnyValue(format)), "$convert")
	}
	if !binDataFormats[name] {
		return "", handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConvertFormatInvalid,
			"Invalid 'format' argument for $convert: "+name, "$convert")
	}
	return name, nil
}

// inputTypeCode is Value::getType for a non-nullish value.
func inputTypeCode(v any) int {
	switch v.(type) {
	case float64:
		return bsonDouble
	case string:
		return bsonString
	case *types.Document:
		return bsonObject
	case *types.Array:
		return bsonArray
	case types.Binary:
		return bsonBinData
	case types.ObjectID:
		return bsonObjectID
	case bool:
		return bsonBool
	case time.Time:
		return bsonDate
	case types.Regex:
		return bsonRegex
	case int32:
		return bsonInt
	case types.Timestamp:
		return bsonTimestamp
	case int64:
		return bsonLong
	case types.Decimal128:
		return bsonDecimal
	case types.MinKeyType:
		return bsonMinKey
	default:
		return bsonMaxKey
	}
}

func unsupportedConversion(v any, target int) error {
	return conversionError(fmt.Sprintf("Unsupported conversion from %s to %s in $convert with no onError value",
		bsonTypeNames[inputTypeCode(v)], bsonTypeNames[target]))
}

// convertValue is ConversionTable::findConversionFunc applied to v.
func convertValue(v any, target int, format string, subtype any) (any, error) {
	switch target {
	case bsonBool:
		return convertToBool(v), nil
	case bsonDouble:
		return convertToDouble(v, target)
	case bsonString:
		return convertToString(v, target, format)
	case bsonObjectID:
		switch v.(type) {
		case string, types.ObjectID:
			return convertToObjectID(v)
		}
	case bsonDate:
		return convertToDate(v, target)
	case bsonInt:
		return convertToInt(v, target)
	case bsonLong:
		return convertToLong(v, target)
	case bsonDecimal:
		return convertToDecimal(v, target)
	case bsonBinData:
		return convertToBinData(v, target, format, subtype)
	}
	return nil, unsupportedConversion(v, target)
}

// convertToBool is Value::coerceToBool; every type converts.
func convertToBool(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case int32:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case types.Decimal128:
		return !decimalIsZero(v)
	default:
		return true
	}
}

func convertToDouble(v any, target int) (any, error) {
	switch v := v.(type) {
	case float64:
		return v, nil
	case string:
		return parseDoubleString(v)
	case bool:
		if v {
			return 1.0, nil
		}
		return 0.0, nil
	case time.Time:
		return float64(v.UnixMilli()), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case types.Decimal128:
		return decimalToDouble(v)
	}
	return nil, unsupportedConversion(v, target)
}

func convertToString(v any, target int, format string) (any, error) {
	switch v := v.(type) {
	case float64:
		return formatDoubleForString(v), nil
	case string:
		return v, nil
	case types.Binary:
		if format == "" {
			return nil, formatRequiredError(bsonBinData, bsonString)
		}
		return binDataToString(v, format)
	case types.ObjectID:
		return hex.EncodeToString(v[:]), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case time.Time:
		return mongoTimeZone{}.formatDate("%Y-%m-%dT%H:%M:%S.%LZ", v.UnixMilli())
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case types.Decimal128:
		return bson.NewDecimal128(v.H, v.L).String(), nil
	}
	return nil, unsupportedConversion(v, target)
}

func convertToDate(v any, target int) (any, error) {
	var ms int64
	switch v := v.(type) {
	case float64:
		n, err := doubleToLong(v)
		if err != nil {
			return nil, err
		}
		ms = n
	case int64:
		ms = v
	case types.Decimal128:
		n, err := decimalToInteger(v, math.MinInt64, math.MaxInt64)
		if err != nil {
			return nil, err
		}
		ms = n
	case string:
		return parseDateString(v, mongoTimeZone{})
	case types.ObjectID:
		ms = int64(int32(binary.BigEndian.Uint32(v[:4]))) * 1000
	case time.Time:
		return v, nil
	case types.Timestamp:
		ms = int64(uint64(v)>>32) * 1000
	default:
		return nil, unsupportedConversion(v, target)
	}
	return time.UnixMilli(ms).UTC(), nil
}

func convertToInt(v any, target int) (any, error) {
	switch v := v.(type) {
	case float64:
		if err := checkFinite(v); err != nil {
			return nil, err
		}
		if v < math.MinInt32 || v > math.MaxInt32 {
			return nil, overflowError(strconv.FormatFloat(v, 'g', 6, 64))
		}
		return int32(v), nil
	case string:
		n, err := parseIntegerString(v, math.MinInt32, math.MaxInt32)
		if err != nil {
			return nil, err
		}
		return int32(n), nil
	case bool:
		if v {
			return int32(1), nil
		}
		return int32(0), nil
	case int32:
		return v, nil
	case int64:
		if v < math.MinInt32 || v > math.MaxInt32 {
			return nil, overflowError("")
		}
		return int32(v), nil
	case types.Decimal128:
		n, err := decimalToInteger(v, math.MinInt32, math.MaxInt32)
		if err != nil {
			return nil, err
		}
		return int32(n), nil
	}
	return nil, unsupportedConversion(v, target)
}

func convertToLong(v any, target int) (any, error) {
	switch v := v.(type) {
	case float64:
		return doubleToLong(v)
	case string:
		return parseIntegerString(v, math.MinInt64, math.MaxInt64)
	case bool:
		if v {
			return int64(1), nil
		}
		return int64(0), nil
	case time.Time:
		return v.UnixMilli(), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case types.Decimal128:
		return decimalToInteger(v, math.MinInt64, math.MaxInt64)
	}
	return nil, unsupportedConversion(v, target)
}

func convertToDecimal(v any, target int) (any, error) {
	switch v := v.(type) {
	case float64:
		return doubleToDecimal(v), nil
	case string:
		return parseDecimalString(v)
	case bool:
		if v {
			return aggregations.APDToDecimal128(apd.New(1, 0)), nil
		}
		return aggregations.APDToDecimal128(apd.New(0, 0)), nil
	case time.Time:
		return aggregations.APDToDecimal128(apd.New(v.UnixMilli(), 0)), nil
	case int32:
		return aggregations.APDToDecimal128(apd.New(int64(v), 0)), nil
	case int64:
		return aggregations.APDToDecimal128(apd.New(v, 0)), nil
	case types.Decimal128:
		return v, nil
	}
	return nil, unsupportedConversion(v, target)
}

func convertToBinData(v any, target int, format string, subtype any) (any, error) {
	switch v := v.(type) {
	case string:
		if format == "" {
			return nil, formatRequiredError(bsonString, bsonBinData)
		}
		binDataType, err := computeBinDataType(subtype)
		if err != nil {
			return nil, err
		}
		return stringToBinData(v, format, binDataType)
	case types.Binary:
		binDataType, err := computeBinDataType(subtype)
		if err != nil {
			return nil, err
		}
		return types.Binary{B: v.B, Subtype: binDataType}, nil
	}
	return nil, unsupportedConversion(v, target)
}

func formatRequiredError(from, to int) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConvertFormatRequired,
		fmt.Sprintf("Format must be speficied when converting from '%s' to '%s'", bsonTypeNames[from], bsonTypeNames[to]),
		"$convert")
}

func overflowError(value string) error {
	return conversionError("Conversion would overflow target type in $convert with no onError value: " + value)
}

func checkFinite(f float64) error {
	if math.IsNaN(f) {
		return conversionError("Attempt to convert NaN value to integer type in $convert with no onError value")
	}
	if math.IsInf(f, 0) {
		return conversionError("Attempt to convert infinity value to integer type in $convert with no onError value")
	}
	return nil
}

// doubleToLong is performCastDoubleToLong.
func doubleToLong(f float64) (int64, error) {
	if err := checkFinite(f); err != nil {
		return 0, err
	}
	if f < math.MinInt64 || f >= 1<<63 {
		return 0, overflowError(strconv.FormatFloat(f, 'g', 6, 64))
	}
	return int64(f), nil
}

func decimalIsZero(d types.Decimal128) bool {
	x := aggregations.DecimalToAPD(d)
	return x.Form == apd.Finite && x.IsZero()
}

// decimalToInteger is performCastDecimalToInt: truncation toward zero.
func decimalToInteger(d types.Decimal128, lo, hi int64) (int64, error) {
	x := aggregations.DecimalToAPD(d)
	switch x.Form {
	case apd.NaN, apd.NaNSignaling:
		return 0, conversionError("Attempt to convert NaN value to integer type in $convert with no onError value")
	case apd.Infinite:
		return 0, conversionError("Attempt to convert infinity value to integer type in $convert with no onError value")
	}
	coeff := new(big.Int).Set(x.Coeff.MathBigInt())
	if x.Exponent > 0 {
		if x.Exponent > 20 && coeff.Sign() != 0 {
			return 0, overflowError(bson.NewDecimal128(d.H, d.L).String())
		}
		coeff.Mul(coeff, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(x.Exponent)), nil))
	} else if x.Exponent < 0 {
		coeff.Quo(coeff, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-x.Exponent)), nil))
	}
	if x.Negative {
		coeff.Neg(coeff)
	}
	if !coeff.IsInt64() || coeff.Int64() < lo || coeff.Int64() > hi {
		return 0, overflowError(bson.NewDecimal128(d.H, d.L).String())
	}
	return coeff.Int64(), nil
}

// decimalToDouble is performCastDecimalToDouble: rounding is allowed, overflow
// and underflow are not.
func decimalToDouble(d types.Decimal128) (any, error) {
	x := aggregations.DecimalToAPD(d)
	switch x.Form {
	case apd.NaN, apd.NaNSignaling:
		return math.NaN(), nil
	case apd.Infinite:
		if x.Negative {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	}
	if x.IsZero() {
		return math.Copysign(0, float64(int64(d.H))), nil
	}
	f, err := strconv.ParseFloat(x.String(), 64)
	tiny := !x.IsZero() && math.Abs(f) < 0x1p-1022
	if err != nil || math.IsInf(f, 0) || tiny {
		return nil, overflowError(bson.NewDecimal128(d.H, d.L).String())
	}
	return f, nil
}

// doubleToDecimal is Decimal128(double) with kRoundTo15Digits.
func doubleToDecimal(f float64) types.Decimal128 {
	if f == 0 {
		zero := "0"
		if math.Signbit(f) {
			zero = "-0"
		}
		b := must.NotFail(bson.ParseDecimal128(zero))
		h, l := b.GetBytes()
		return types.Decimal128{H: h, L: l}
	}
	return aggregations.APDToDecimal128(aggregations.DoubleToAPD(f))
}

// formatDoubleForString is performFormatDouble: fmt's shortest round-trip
// form, fixed notation for decimal exponents in [-4, 16).
func formatDoubleForString(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	case f == 0 && math.Signbit(f):
		return "-0"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(sci[strings.IndexByte(sci, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return sci
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func parseNumberError(s, reason string) error {
	return conversionError(fmt.Sprintf("Failed to parse number '%s' in $convert with no onError value: %s", s, reason))
}

// parseIntegerString is NumberParser with base 10: an optional sign, then
// digits only.
func parseIntegerString(s string, lo, hi int64) (int64, error) {
	if strings.HasPrefix(s, "0x") {
		return 0, conversionError("Illegal hexadecimal input in $convert with no onError value: " + s)
	}
	digits := s
	negative := false
	if digits != "" && (digits[0] == '-' || digits[0] == '+') {
		negative = digits[0] == '-'
		digits = digits[1:]
	}
	if digits == "" {
		return 0, parseNumberError(s, "No digits")
	}
	magnitude := new(big.Int)
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if c < '0' || c > '9' {
			if i == 0 {
				return 0, parseNumberError(s, "Did not consume any digits")
			}
			return 0, parseNumberError(s, "Did not consume whole string.")
		}
		magnitude.Mul(magnitude, big.NewInt(10))
		magnitude.Add(magnitude, big.NewInt(int64(c-'0')))
	}
	if negative {
		magnitude.Neg(magnitude)
	}
	if !magnitude.IsInt64() || magnitude.Int64() < lo || magnitude.Int64() > hi {
		return 0, parseNumberError(s, "Overflow")
	}
	return magnitude.Int64(), nil
}

// parseDoubleString is NumberParser for a double, which is glibc strtod over
// the whole string without leading whitespace.
func parseDoubleString(s string) (any, error) {
	if strings.HasPrefix(s, "0x") {
		return nil, conversionError("Illegal hexadecimal input in $convert with no onError value: " + s)
	}
	if s == "" {
		return nil, parseNumberError(s, "Empty string")
	}
	if isSpace(s[0]) {
		return nil, parseNumberError(s, "Leading whitespace")
	}

	body := s
	sign := ""
	if body[0] == '+' || body[0] == '-' {
		sign, body = body[:1], body[1:]
	}
	lower := strings.ToLower(body)
	switch {
	case lower == "inf" || lower == "infinity":
		return strconv.ParseFloat(sign+"Inf", 64)
	case lower == "nan" || (strings.HasPrefix(lower, "nan(") && strings.HasSuffix(lower, ")") && isNaNChars(lower[4:len(lower)-1])):
		return math.NaN(), nil
	case strings.HasPrefix(lower, "0x"):
		return parseHexFloat(s, sign, body[2:])
	}

	nonZero, ok := scanDecimalFloat(body)
	if !ok {
		return nil, parseNumberError(s, "Did not consume whole string.")
	}
	f, err := strconv.ParseFloat(sign+body, 64)
	if err != nil || (nonZero && math.Abs(f) < 0x1p-1022) {
		return nil, parseNumberError(s, "Out of range")
	}
	return f, nil
}

func isSpace(c byte) bool {
	return c == ' ' || (c >= '\t' && c <= '\r')
}

func isNaNChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c == '_') {
			return false
		}
	}
	return true
}

// scanDecimalFloat reports whether s is digits with an optional point and
// exponent, and whether any mantissa digit is non-zero.
func scanDecimalFloat(s string) (nonZero, ok bool) {
	i, mantissaDigits := 0, 0
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		mantissaDigits++
		nonZero = nonZero || s[i] != '0'
	}
	if i < len(s) && s[i] == '.' {
		for i++; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
			mantissaDigits++
			nonZero = nonZero || s[i] != '0'
		}
	}
	if mantissaDigits == 0 {
		return false, false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		start := j
		for ; j < len(s) && s[j] >= '0' && s[j] <= '9'; j++ {
		}
		if j > start {
			i = j
		}
	}
	return nonZero, i == len(s)
}

// parseHexFloat parses strtod's hexadecimal form: hex digits, an optional
// point and an optional binary exponent.
func parseHexFloat(s, sign, digits string) (any, error) {
	mantissa, exponent := digits, "0"
	if i := strings.IndexAny(digits, "pP"); i >= 0 {
		mantissa, exponent = digits[:i], digits[i+1:]
	}
	valid := mantissa != "" && mantissa != "."
	for i := 0; i < len(mantissa) && valid; i++ {
		c := mantissa[i]
		valid = c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	}
	if !valid || strings.Count(mantissa, ".") > 1 {
		return nil, parseNumberError(s, "Did not consume whole string.")
	}
	f, err := strconv.ParseFloat(sign+"0x"+mantissa+"p"+exponent, 64)
	if err != nil {
		return nil, parseNumberError(s, "Out of range")
	}
	return f, nil
}

// decimalParseContext rounds a parsed string to Decimal128.
var decimalParseContext = apd.Context{
	Precision:   34,
	MaxExponent: 6144,
	MinExponent: -6143,
	Rounding:    apd.RoundHalfEven,
}

// parseDecimalString is NumberParser for a Decimal128: rounding is allowed,
// overflow and underflow are not.
func parseDecimalString(s string) (any, error) {
	if s == "" {
		return nil, parseNumberError(s, "Empty string")
	}
	body := strings.TrimLeft(s, "+-")
	if len(s)-len(body) > 1 {
		return nil, parseNumberError(s, "Failed to parse string to decimal")
	}
	switch strings.ToLower(body) {
	case "inf", "infinity", "nan":
	default:
		if _, ok := scanDecimalFloat(body); !ok {
			return nil, parseNumberError(s, "Failed to parse string to decimal")
		}
	}

	x, _, err := apd.NewFromString(s)
	if err != nil {
		return nil, parseNumberError(s, "Failed to parse string to decimal")
	}
	if x.Form != apd.Finite {
		return aggregations.APDToDecimal128(x), nil
	}
	rounded := new(apd.Decimal)
	cond, _ := decimalParseContext.Round(rounded, x)
	switch {
	case cond.Overflow():
		return nil, parseNumberError(s, "Conversion from string to decimal would overflow")
	case cond.Underflow():
		return nil, parseNumberError(s, "Conversion from string to decimal would underflow")
	}
	return aggregations.APDToDecimal128(rounded), nil
}

// computeBinDataType is ConversionTable::computeBinDataType.
func computeBinDataType(subtype any) (types.BinarySubtype, error) {
	if !isNumber(subtype) {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConvertSubtypeType,
			"For BinData, $convert's 'subtype' argument must be a number, but is "+handlerparams.AliasFromType(subtype), "$convert")
	}
	code, integral := integralInt(subtype)
	if !integral {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConvertSubtypeNotInteger,
			"In $convert, numeric 'subtype' argument is not an integer", "$convert")
	}
	if !(code >= 0 && code <= 8 || code >= binDataUserMin && code <= 255) {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConvertSubtypeInvalid,
			fmt.Sprintf("In $convert, numeric value for 'subtype' does not correspond to a BinData type: %d", code), "$convert")
	}
	if code == binDataColumn {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConvertSubtypeColumn,
			"$convert to BinData subtype Column (7) is not allowed", "$convert")
	}
	return types.BinarySubtype(code), nil
}

// errorReason returns the message of a command error.
func errorReason(err error) string {
	var cmdErr *handlererrors.CommandError
	if errors.As(err, &cmdErr) {
		return cmdErr.Err().Error()
	}
	return err.Error()
}

// stringToBinData is parseStringToBinData; every failure is a conversion
// failure.
func stringToBinData(s, format string, binDataType types.BinarySubtype) (any, error) {
	b, err := decodeBinData(s, format, binDataType)
	if err != nil {
		return nil, conversionError(fmt.Sprintf("Failed to parse BinData '%s' in $convert with no onError value: %s",
			s, errorReason(err)))
	}
	return b, nil
}

func decodeBinData(s, format string, binDataType types.BinarySubtype) (types.Binary, error) {
	if (format == "uuid") != (binDataType == binDataUUID) {
		return types.Binary{}, errors.New("Only the 'uuid' format is allowed with the UUID subtype")
	}

	var b []byte
	var err error
	switch format {
	case "base64":
		b, err = base64.StdEncoding.DecodeString(s)
	case "base64url":
		b, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	case "hex":
		b, err = hex.DecodeString(s)
	case "utf8":
		if !utf8.ValidString(s) {
			err = errors.New("Invalid UTF-8: " + s)
		}
		b = []byte(s)
	case "uuid":
		b, err = parseUUID(s)
	default:
		err = fmt.Errorf("Invalid format '%s'", format)
	}
	if err != nil {
		return types.Binary{}, err
	}
	return types.Binary{B: b, Subtype: binDataType}, nil
}

// parseUUID is UUID::parse: 8-4-4-4-12 hex digits.
func parseUUID(s string) ([]byte, error) {
	invalid := fmt.Errorf("Invalid UUID string: %s", s)
	if len(s) != 36 {
		return nil, invalid
	}
	var compact strings.Builder
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return nil, invalid
			}
			continue
		}
		compact.WriteByte(s[i])
	}
	b, err := hex.DecodeString(compact.String())
	if err != nil {
		return nil, invalid
	}
	return b, nil
}

func formatUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// binDataToString is performConvertBinDataToString.
func binDataToString(b types.Binary, format string) (any, error) {
	isUUID := b.Subtype == binDataUUID && len(b.B) == 16
	fail := func(reason string) (any, error) {
		return nil, conversionError(fmt.Sprintf("Failed to convert '%s' to string in $convert with no onError value: %s",
			types.FormatAnyValue(b), reason))
	}
	switch format {
	case "auto":
		if isUUID {
			return formatUUID(b.B), nil
		}
		return base64.StdEncoding.EncodeToString(b.B), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(b.B), nil
	case "base64url":
		return base64.RawURLEncoding.EncodeToString(b.B), nil
	case "hex":
		return hex.EncodeToString(b.B), nil
	case "utf8":
		if !utf8.Valid(b.B) {
			return fail("BinData does not represent a valid UTF-8 string")
		}
		return string(b.B), nil
	case "uuid":
		if !isUUID {
			return fail("BinData does not represent a valid UUID")
		}
		return formatUUID(b.B), nil
	}
	return fail(fmt.Sprintf("Invalid format '%s'", format))
}

var _ Operator = (*convertOp)(nil)
