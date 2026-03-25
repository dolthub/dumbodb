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

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/dolthub/dongo/internal/types"
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
	// TODO https://github.com/dolthub/dongo/issues/2300
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
		// to avoid exceeding primitive.ParseDecimal128's 35-digit limit.
		bf := new(big.Float).SetPrec(128).SetFloat64(v)
		s := bf.Text('g', 34)
		p, err := primitive.ParseDecimal128(s)

		if err != nil {
			return decimalVal{}, false
		}

		m, exp, err := p.BigInt()
		if err != nil {
			return decimalVal{}, false
		}

		return decimalVal{m, exp}, true
	case types.Decimal128:
		p := primitive.NewDecimal128(v.H, v.L)
		m, exp, err := p.BigInt()

		if err != nil {
			// NaN or Inf — skip
			return decimalVal{}, false
		}

		return decimalVal{m, exp}, true
	default:
		_ = ten
		return decimalVal{}, false
	}
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
		p, _ := primitive.ParseDecimal128FromBigInt(big.NewInt(0), 0)
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
	p, ok := primitive.ParseDecimal128FromBigInt(totalM, minExp)
	if !ok {
		// Overflow — return Decimal128 zero as fallback.
		p, _ = primitive.ParseDecimal128FromBigInt(big.NewInt(0), 0)
	}

	h, l := p.GetBytes()

	return types.Decimal128{H: h, L: l}
}
