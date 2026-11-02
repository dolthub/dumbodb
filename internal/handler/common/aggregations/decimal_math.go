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

package aggregations

import (
	"math/big"
	"strconv"

	"github.com/cockroachdb/apd/v3"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/types"
)

// decimalWorkContext computes Decimal128 math with digits to spare, so a
// result can be rounded to Decimal128's 34 digits and checked for exactness.
var decimalWorkContext = apd.Context{
	Precision:   70,
	MaxExponent: apd.MaxExponent,
	MinExponent: apd.MinExponent,
	Rounding:    apd.RoundHalfEven,
}

// DecimalWorkContext is the context for Decimal128 math; round its results
// with DecimalResult or DecimalFullPrecisionResult.
func DecimalWorkContext() *apd.Context {
	c := decimalWorkContext
	return &c
}

// DecimalToAPD converts d, including NaN and infinities.
func DecimalToAPD(d types.Decimal128) *apd.Decimal {
	b := bson.NewDecimal128(d.H, d.L)
	switch {
	case b.IsNaN():
		return &apd.Decimal{Form: apd.NaN}
	case b.IsInf() != 0:
		return &apd.Decimal{Form: apd.Infinite, Negative: b.IsInf() < 0}
	}
	out, ok := decimalToAPD(d)
	if !ok {
		return &apd.Decimal{Form: apd.NaN}
	}
	return out
}

// DoubleToAPD converts f to decimal with 15 significant digits, as MongoDB
// does when a double meets a Decimal128.
func DoubleToAPD(f float64) *apd.Decimal {
	out, _, err := apd.NewFromString(strconv.FormatFloat(f, 'e', 14, 64))
	if err != nil {
		switch {
		case f > 0:
			return &apd.Decimal{Form: apd.Infinite}
		case f < 0:
			return &apd.Decimal{Form: apd.Infinite, Negative: true}
		default:
			return &apd.Decimal{Form: apd.NaN}
		}
	}
	return out
}

// NumberToAPD converts an int32, int64, float64 or Decimal128; ok is false for
// any other value.
func NumberToAPD(v any) (*apd.Decimal, bool) {
	switch n := v.(type) {
	case int32:
		return apd.New(int64(n), 0), true
	case int64:
		return apd.New(n, 0), true
	case float64:
		return DoubleToAPD(n), true
	case types.Decimal128:
		return DecimalToAPD(n), true
	default:
		return nil, false
	}
}

// APDToDecimal128 rounds x to Decimal128.
func APDToDecimal128(x *apd.Decimal) types.Decimal128 {
	var b bson.Decimal128
	switch x.Form {
	case apd.NaN, apd.NaNSignaling:
		b, _ = bson.ParseDecimal128("NaN")
	case apd.Infinite:
		if x.Negative {
			b, _ = bson.ParseDecimal128("-Infinity")
		} else {
			b, _ = bson.ParseDecimal128("Infinity")
		}
	default:
		rounded := new(apd.Decimal)
		_, _ = decimal128Context.Round(rounded, x)
		coeff := rounded.Coeff.MathBigInt()
		if rounded.Negative {
			coeff.Neg(coeff)
		}
		var ok bool
		if b, ok = bson.ParseDecimal128FromBigInt(coeff, int(rounded.Exponent)); !ok {
			if rounded.Negative {
				b, _ = bson.ParseDecimal128("-Infinity")
			} else {
				b, _ = bson.ParseDecimal128("Infinity")
			}
		}
	}
	h, l := b.GetBytes()
	return types.Decimal128{H: h, L: l}
}

// DecimalResult rounds x, computed with DecimalWorkContext, the way MongoDB
// reports $sqrt, $exp, $ln, $log and $log10: an exact result in its shortest
// form at no less than idealExponent (sqrt(4) is 2, sqrt(100) is 10), an
// inexact one with all 34 digits (log10(2) keeps a trailing zero).
func DecimalResult(x *apd.Decimal, idealExponent int32) types.Decimal128 {
	if x.Form != apd.Finite {
		return APDToDecimal128(x)
	}

	check := apd.Context{Precision: 50, MaxExponent: apd.MaxExponent, MinExponent: apd.MinExponent, Rounding: apd.RoundHalfEven}
	at50 := new(apd.Decimal)
	_, _ = check.Round(at50, x)
	reduced := new(apd.Decimal)
	reduced.Reduce(at50)
	if reduced.NumDigits() > 34 {
		return APDToDecimal128(x)
	}

	if reduced.Exponent > idealExponent && !reduced.IsZero() {
		scale := reduced.Exponent - idealExponent
		if reduced.NumDigits()+int64(scale) <= 34 {
			coeff := new(big.Int).Mul(reduced.Coeff.MathBigInt(), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
			reduced.Coeff.SetMathBigInt(coeff)
			reduced.Exponent = idealExponent
		}
	}
	if reduced.IsZero() {
		reduced.Exponent = idealExponent
		reduced.Negative = false
	}
	return APDToDecimal128(reduced)
}

// DecimalFullPrecisionResult rounds x to 34 significant digits, padding an
// exact result with zeros, as MongoDB reports $pow (2 to the 3rd is
// 8.000000000000000000000000000000000).
func DecimalFullPrecisionResult(x *apd.Decimal) types.Decimal128 {
	if x.Form != apd.Finite || x.IsZero() {
		return APDToDecimal128(x)
	}

	rounded := new(apd.Decimal)
	_, _ = decimal128Context.Round(rounded, x)
	if digits := rounded.NumDigits(); digits < 34 {
		pad := 34 - digits
		coeff := new(big.Int).Mul(rounded.Coeff.MathBigInt(), new(big.Int).Exp(big.NewInt(10), big.NewInt(pad), nil))
		rounded.Coeff.SetMathBigInt(coeff)
		rounded.Exponent -= int32(pad)
	}
	return APDToDecimal128(rounded)
}

// DecimalMultiply returns x*y rounded to Decimal128's 34 digits, as
// Decimal128::multiply does.
func DecimalMultiply(x, y *apd.Decimal) *apd.Decimal {
	product := new(apd.Decimal)
	_, _ = decimal128Context.Mul(product, x, y)
	return product
}

// DecimalAdd returns x+y rounded to Decimal128's 34 digits, as
// Decimal128::add does.
func DecimalAdd(x, y *apd.Decimal) *apd.Decimal {
	sum := new(apd.Decimal)
	_, _ = decimal128Context.Add(sum, x, y)
	return sum
}
