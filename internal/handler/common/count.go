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
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/cockroachdb/apd/v3"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

type CountParams struct {
	Filter     *types.Document `dumbo:"query,opt"`
	DB         string          `dumbo:"$db"`
	Collection string          `dumbo:"count,collection"`

	SkipRaw  any   `dumbo:"skip,opt"`
	LimitRaw any   `dumbo:"limit,opt"`
	Skip     int64 `dumbo:"-"`
	Limit    int64 `dumbo:"-"`

	Collation *types.Document `dumbo:"collation,opt"`

	Fields any `dumbo:"fields,ignored"` // legacy MongoDB shell adds it, but it is never actually used

	MaxTimeMSValue any             `dumbo:"maxTimeMS,opt"`
	MaxTimeMS      int64           `dumbo:"-"`
	Hint           any             `dumbo:"hint,opt"`
	ReadConcern    *types.Document `dumbo:"readConcern,ignored"`
	Comment        string          `dumbo:"comment,ignored"`
	LSID           any             `dumbo:"lsid,ignored"`
	ClusterTime    any             `dumbo:"$clusterTime,ignored"`
	ReadPreference *types.Document `dumbo:"$readPreference,ignored"`

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`
}

func GetCountParams(document *types.Document, l *slog.Logger) (*CountParams, error) {
	var count CountParams

	err := handlerparams.ExtractParams(document, "count", &count, l)
	if err == nil {
		count.MaxTimeMS, err = handlerparams.CommandMaxTimeMS(count.MaxTimeMSValue)
	}
	if err != nil {
		return nil, err
	}

	if count.SkipRaw != nil {
		if count.Skip, err = ParseCountSkip(count.SkipRaw); err != nil {
			return nil, err
		}
	}
	if count.LimitRaw != nil {
		if count.Limit, err = ParseCountLimit(count.LimitRaw); err != nil {
			return nil, err
		}
	}

	return &count, nil
}

// ParseCountLimit parses count's limit as mongod does: a whole number whose
// sign is ignored, since a negative limit is a limit too.
func ParseCountLimit(v any) (int64, error) {
	var n int64
	switch v := v.(type) {
	case int32:
		n = int64(v)
	case int64:
		n = v
	case float64:
		switch {
		case math.IsNaN(v):
			return 0, countParseError("Expected an integer, but found NaN in: limit: nan.0")
		case v >= math.MaxInt64 || v < math.MinInt64:
			return 0, countParseError("Cannot represent as a 64-bit integer: limit: " + countDoubleString(v))
		case v != math.Trunc(v):
			return 0, countParseError("Expected an integer: limit: " + countDoubleString(v))
		}
		n = int64(v)
	case types.Decimal128:
		d, ok := decimalToIntegral(v, false)
		if !ok {
			return 0, countParseError("Cannot represent as a 64-bit integer: limit: " + decimalString(v))
		}
		n = d
	default:
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "limit value is not a valid number", "count",
		)
	}
	if n == math.MinInt64 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "limit value for count cannot be min long", "count",
		)
	}
	if n < 0 {
		n = -n
	}
	return n, nil
}

// ParseCountSkip parses count's skip as mongod does: a double is truncated, a
// Decimal128 is rounded half to even, both saturate, and null is zero.
func ParseCountSkip(v any) (int64, error) {
	var n int64
	switch v := v.(type) {
	case types.NullType:
		return 0, nil
	case int32:
		n = int64(v)
	case int64:
		n = v
	case float64:
		switch {
		case math.IsNaN(v):
			n = 0
		case v >= math.MaxInt64:
			n = math.MaxInt64
		case v <= math.MinInt64:
			n = math.MinInt64
		default:
			n = int64(v)
		}
	case types.Decimal128:
		d, ok := decimalToIntegral(v, true)
		if !ok {
			return 0, nil
		}
		n = d
	default:
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"BSON field 'count.skip' is the wrong type '"+handlerparams.AliasFromType(v)+
				"', expected types '[double, decimal, long, int']",
			"count",
		)
	}
	if n < 0 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("BSON field 'skip' value must be >= 0, actual value '%d'", n),
			"count",
		)
	}
	return n, nil
}

func countParseError(msg string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse, msg, "count")
}

// decimalToIntegral returns d as an int64. With round, a fraction is rounded
// half to even and an out-of-range value saturates; without it, ok is false
// for either. ok is false for NaN.
func decimalToIntegral(d types.Decimal128, round bool) (int64, bool) {
	coeff, exp, err := primitive.NewDecimal128(d.H, d.L).BigInt()
	if err != nil {
		if !round {
			return 0, false
		}
		s := decimalString(d)
		switch {
		case strings.HasPrefix(s, "-Inf"):
			return math.MinInt64, true
		case strings.HasPrefix(s, "Inf"):
			return math.MaxInt64, true
		}
		return 0, false
	}
	a := apd.NewWithBigInt(new(apd.BigInt).SetMathBigInt(coeff), int32(exp))
	var integral apd.Decimal
	ctx := apd.BaseContext.WithPrecision(6200)
	ctx.Rounding = apd.RoundHalfEven
	if _, err = ctx.RoundToIntegralValue(&integral, a); err != nil {
		return 0, false
	}
	if !round && integral.Cmp(a) != 0 {
		return 0, false
	}
	n, err := integral.Int64()
	if err == nil {
		return n, true
	}
	if !round {
		return 0, false
	}
	if integral.Negative {
		return math.MinInt64, true
	}
	return math.MaxInt64, true
}

func decimalString(d types.Decimal128) string {
	return primitive.NewDecimal128(d.H, d.L).String()
}

// countDoubleString formats f as mongod does in count's parse errors.
func countDoubleString(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
