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
	"math"
	"math/big"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/types"
)

// SumNumbers accumulate numbers and returns the result of summation.
// The result has the same type as the input, except when the result
// cannot be presented accurately. Then int32 is converted to int64,
// and int64 is converted to float64. It ignores non-number values.
// If any input is Decimal128, all values are summed as Decimal128 and
// the result is Decimal128.
// For empty `vs`, it returns int32(0).
// This should only be used for aggregation, aggregation does not return
// error on overflow.
func SumNumbers(vs ...any) any {
	// Check if any input is Decimal128; if so, delegate to decimal128 sum.
	for _, v := range vs {
		if _, ok := v.(types.Decimal128); ok {
			return sumDecimal128(vs)
		}
	}

	// use big.Int to accumulate values larger than math.MaxInt64.
	intSum := big.NewInt(0)

	// handle accumulation of doubles close to max precision.
	var floatSum float64

	var hasFloat64, hasInt64 bool

	for _, v := range vs {
		switch v := v.(type) {
		case float64:
			hasFloat64 = true

			floatSum = floatSum + v
		case int32:
			intSum.Add(intSum, big.NewInt(int64(v)))
		case int64:
			hasInt64 = true

			intSum.Add(intSum, big.NewInt(v))
		default:
			// ignore non-number
		}
	}

	if hasFloat64 || !intSum.IsInt64() {
		// ignore accuracy because there is no rounding from int64.
		intAsFloat, _ := new(big.Float).SetInt(intSum).Float64()

		return intAsFloat + floatSum
	}

	integer := intSum.Int64()

	if !hasInt64 && integer <= math.MaxInt32 && integer >= math.MinInt32 {
		// convert to int32 if input has no int64 and can be represented in int32.
		return int32(integer)
	}

	return integer
}

// decimalVal holds a number as a (mantissa, exponent) pair where value = mantissa * 10^exponent.
type decimalVal struct {
	m   *big.Int
	exp int
}

// toDecimalVal converts a numeric value to (mantissa, exponent) representation.
// Returns (zero, 0, false) for non-numeric or special (NaN/Inf) values.
func toDecimalVal(v any) (decimalVal, bool) {
	ten := big.NewInt(10)

	switch v := v.(type) {
	case int32:
		return decimalVal{big.NewInt(int64(v)), 0}, true
	case int64:
		return decimalVal{big.NewInt(v), 0}, true
	case float64:
		// Convert float64 to its Decimal128 representation.
		// Use 34 significant digits (Decimal128's max precision) in 'g' format
		// to avoid exceeding bson.ParseDecimal128's 35-digit limit.
		bf := new(big.Float).SetPrec(128).SetFloat64(v)
		s := bf.Text('g', 34)
		p, err := bson.ParseDecimal128(s)

		if err != nil {
			return decimalVal{}, false
		}

		m, exp, err := p.BigInt()
		if err != nil {
			return decimalVal{}, false
		}

		return decimalVal{m, exp}, true
	case types.Decimal128:
		p := bson.NewDecimal128(v.H, v.L)
		m, exp, err := p.BigInt()

		if err != nil {
			// NaN or Inf  -- skip
			return decimalVal{}, false
		}

		return decimalVal{m, exp}, true
	default:
		_ = ten
		return decimalVal{}, false
	}
}

// AvgNumbers computes the average of numeric values.
// If any input is Decimal128, the average is computed and returned as Decimal128.
// Otherwise it returns float64, or types.Null when there are no numeric values.
func AvgNumbers(vs ...any) any {
	for _, v := range vs {
		if _, ok := v.(types.Decimal128); ok {
			return avgDecimal128(vs)
		}
	}

	var sum float64
	var count int

	for _, v := range vs {
		switch v := v.(type) {
		case float64:
			sum += v
			count++
		case int32:
			sum += float64(v)
			count++
		case int64:
			sum += float64(v)
			count++
		}
	}

	if count == 0 {
		return types.Null
	}

	return sum / float64(count)
}

// avgDecimal128 computes the average of numeric values using Decimal128 arithmetic.
// Called when at least one element is types.Decimal128.
func avgDecimal128(vs []any) types.Decimal128 {
	var count int64

	for _, v := range vs {
		switch v.(type) {
		case int32, int64, float64, types.Decimal128:
			count++
		}
	}

	if count == 0 {
		p, _ := bson.ParseDecimal128FromBigInt(big.NewInt(0), 0)
		h, l := p.GetBytes()

		return types.Decimal128{H: h, L: l}
	}

	sum := sumDecimal128(vs)
	p := bson.NewDecimal128(sum.H, sum.L)

	m, exp, err := p.BigInt()
	if err != nil {
		// NaN or Inf  -- return zero.
		p2, _ := bson.ParseDecimal128FromBigInt(big.NewInt(0), 0)
		h, l := p2.GetBytes()

		return types.Decimal128{H: h, L: l}
	}

	// Divide: scale m up by 10^34 to preserve 34 digits of precision,
	// then integer-divide by count. Result exponent = exp - 34.
	const precisionDigits = 34

	ten := big.NewInt(10)
	factor := new(big.Int).Exp(ten, big.NewInt(precisionDigits), nil)
	scaled := new(big.Int).Mul(m, factor)
	quotient := new(big.Int).Quo(scaled, big.NewInt(count))

	resultExp := exp - precisionDigits

	// Normalize: strip trailing zeros from the mantissa added by the precision
	// scaling step, so the result is compact (e.g. "20" not "20.0000…0").
	// After stripping, re-expand if the exponent would become positive so that
	// "20" is represented as mantissa=20/exp=0 rather than mantissa=2/exp=1
	// (which round-trips as "2E+1" instead of the more readable "20").
	if quotient.Sign() != 0 {
		rem := new(big.Int)

		for {
			q, r := new(big.Int).DivMod(quotient, ten, rem)
			if r.Sign() != 0 {
				break
			}

			quotient = q
			resultExp++
		}

		// If stripping produced a positive exponent, re-expand to exp=0
		// so the decimal driver formats the value without scientific notation.
		if resultExp > 0 {
			factor := new(big.Int).Exp(ten, big.NewInt(int64(resultExp)), nil)
			quotient.Mul(quotient, factor)
			resultExp = 0
		}
	}

	result, ok := bson.ParseDecimal128FromBigInt(quotient, resultExp)
	if !ok {
		result, _ = bson.ParseDecimal128FromBigInt(big.NewInt(0), 0)
	}

	h, l := result.GetBytes()

	return types.Decimal128{H: h, L: l}
}

// MultiplyNumbers multiplies numeric values together, preserving type.
// If any value is Decimal128 the result is Decimal128.
// float64 dominates over int types; int64 dominates over int32.
// Non-numeric values are ignored (treated as identity for multiplication).
func MultiplyNumbers(vs ...any) any {
	for _, v := range vs {
		if _, ok := v.(types.Decimal128); ok {
			return multiplyDecimal128(vs)
		}
	}

	var hasFloat64, hasInt64 bool

	for _, v := range vs {
		switch v.(type) {
		case float64:
			hasFloat64 = true
		case int64:
			hasInt64 = true
		}
	}

	if hasFloat64 {
		result := 1.0

		for _, v := range vs {
			switch v := v.(type) {
			case float64:
				result *= v
			case int32:
				result *= float64(v)
			case int64:
				result *= float64(v)
			}
		}

		return result
	}

	bigResult := big.NewInt(1)

	for _, v := range vs {
		switch v := v.(type) {
		case int32:
			bigResult.Mul(bigResult, big.NewInt(int64(v)))
		case int64:
			bigResult.Mul(bigResult, big.NewInt(v))
		}
	}

	if hasInt64 {
		if bigResult.IsInt64() {
			return bigResult.Int64()
		}

		f, _ := new(big.Float).SetInt(bigResult).Float64()

		return f
	}

	// All int32: return int32 if fits, otherwise promote to int64.
	val := bigResult.Int64()
	if val >= math.MinInt32 && val <= math.MaxInt32 {
		return int32(val)
	}

	return val
}

// multiplyDecimal128 multiplies numeric values using Decimal128 arithmetic.
// Called when at least one element is types.Decimal128.
func multiplyDecimal128(vs []any) types.Decimal128 {
	var decimals []decimalVal

	for _, v := range vs {
		d, ok := toDecimalVal(v)
		if ok {
			decimals = append(decimals, d)
		}
	}

	if len(decimals) == 0 {
		p, _ := bson.ParseDecimal128FromBigInt(big.NewInt(0), 0)
		h, l := p.GetBytes()

		return types.Decimal128{H: h, L: l}
	}

	// result = product(m_i) * 10^(sum(e_i))
	totalM := new(big.Int).Set(decimals[0].m)
	totalExp := decimals[0].exp

	for _, d := range decimals[1:] {
		totalM.Mul(totalM, d.m)
		totalExp += d.exp
	}

	p, ok := bson.ParseDecimal128FromBigInt(totalM, totalExp)
	if !ok {
		p, _ = bson.ParseDecimal128FromBigInt(big.NewInt(0), 0)
	}

	h, l := p.GetBytes()

	return types.Decimal128{H: h, L: l}
}

// sumDecimal128 sums a slice of numbers using Decimal128 arithmetic.
// Called when at least one element is types.Decimal128.
func sumDecimal128(vs []any) types.Decimal128 {
	// Convert all numeric values to (mantissa, exponent) pairs.
	var decimals []decimalVal

	for _, v := range vs {
		d, ok := toDecimalVal(v)
		if ok {
			decimals = append(decimals, d)
		}
	}

	if len(decimals) == 0 {
		// No summable values: return Decimal128 zero.
		p, _ := bson.ParseDecimal128FromBigInt(big.NewInt(0), 0)
		h, l := p.GetBytes()

		return types.Decimal128{H: h, L: l}
	}

	// Find the minimum exponent so all values can be expressed with it.
	minExp := decimals[0].exp
	for _, d := range decimals[1:] {
		if d.exp < minExp {
			minExp = d.exp
		}
	}

	ten := big.NewInt(10)
	totalM := big.NewInt(0)

	for _, d := range decimals {
		// Scale: d.m * 10^(d.exp - minExp)
		scale := d.exp - minExp
		scaled := new(big.Int).Set(d.m)

		if scale > 0 {
			factor := new(big.Int).Exp(ten, big.NewInt(int64(scale)), nil)
			scaled.Mul(scaled, factor)
		}

		totalM.Add(totalM, scaled)
	}

	// totalM * 10^minExp is the result.
	p, ok := bson.ParseDecimal128FromBigInt(totalM, minExp)
	if !ok {
		// Overflow  -- return Decimal128 zero as fallback.
		p, _ = bson.ParseDecimal128FromBigInt(big.NewInt(0), 0)
	}

	h, l := p.GetBytes()

	return types.Decimal128{H: h, L: l}
}
