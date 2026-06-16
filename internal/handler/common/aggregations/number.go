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

	"github.com/cockroachdb/apd/v3"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/types"
)

// decimal128Context divides with IEEE 754-2008 decimal128 semantics.
var decimal128Context = apd.Context{
	Precision:   34,
	MaxExponent: 6144,
	MinExponent: -6143,
	Rounding:    apd.RoundHalfEven,
}

func decimalToAPD(d types.Decimal128) (*apd.Decimal, bool) {
	coeff, exp, err := bson.NewDecimal128(d.H, d.L).BigInt()
	if err != nil {
		return nil, false
	}

	out := new(apd.Decimal)
	out.Form = apd.Finite
	out.Negative = coeff.Sign() < 0
	out.Coeff.SetMathBigInt(new(big.Int).Abs(coeff))
	out.Exponent = int32(exp)

	return out, true
}

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

// decimalZero returns Decimal128 zero.
func decimalZero() types.Decimal128 {
	p, _ := bson.ParseDecimal128FromBigInt(big.NewInt(0), 0)
	h, l := p.GetBytes()

	return types.Decimal128{H: h, L: l}
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
		return decimalZero()
	}

	return avgFromDecimalSum(sumDecimal128(vs), count)
}

// avgFromDecimalSum divides a Decimal128 sum by count, preserving 34 digits
// of precision. Shared by the batch (avgDecimal128) and incremental (NumberAvg)
// paths so they produce identical results.
func avgFromDecimalSum(sum types.Decimal128, count int64) types.Decimal128 {
	dividend, ok := decimalToAPD(sum)
	if !ok {
		return decimalZero()
	}

	quo := new(apd.Decimal)
	cond, err := decimal128Context.Quo(quo, dividend, apd.New(count, 0))
	if err != nil {
		return decimalZero()
	}

	if quo.Form != apd.Finite {
		return decimalZero()
	}

	coeff := quo.Coeff.MathBigInt()
	exp := int(quo.Exponent)

	// Reduce an exact quotient to the IEEE divide ideal exponent (exp(dividend),
	// since count's is 0) so the representation matches MongoDB: 75, not 75.000...0.
	if !cond.Inexact() {
		ideal := int(dividend.Exponent)
		if coeff.Sign() == 0 {
			exp = ideal
		} else {
			ten := big.NewInt(10)
			rem := new(big.Int)

			for exp < ideal {
				q, r := new(big.Int).QuoRem(coeff, ten, rem)
				if r.Sign() != 0 {
					break
				}

				coeff = q
				exp++
			}
		}
	}

	if quo.Negative {
		coeff = new(big.Int).Neg(coeff)
	}

	result, ok := bson.ParseDecimal128FromBigInt(coeff, exp)
	if !ok {
		return decimalZero()
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

// decimalSum incrementally accumulates Decimal128 values, producing the same
// total as sumDecimal128 over the same sequence. Integer-mantissa addition at a
// common minimum exponent is associative, so the incremental and batch results
// match exactly.
type decimalSum struct {
	totalM *big.Int
	minExp int
	any    bool
}

func (s *decimalSum) addDecimalVal(dv decimalVal) {
	if !s.any {
		s.totalM = new(big.Int).Set(dv.m)
		s.minExp = dv.exp
		s.any = true

		return
	}

	ten := big.NewInt(10)

	if dv.exp < s.minExp {
		factor := new(big.Int).Exp(ten, big.NewInt(int64(s.minExp-dv.exp)), nil)
		s.totalM.Mul(s.totalM, factor)
		s.minExp = dv.exp
		s.totalM.Add(s.totalM, dv.m)

		return
	}

	scaled := new(big.Int).Set(dv.m)

	if dv.exp > s.minExp {
		factor := new(big.Int).Exp(ten, big.NewInt(int64(dv.exp-s.minExp)), nil)
		scaled.Mul(scaled, factor)
	}

	s.totalM.Add(s.totalM, scaled)
}

func (s *decimalSum) add(v any) {
	if dv, ok := toDecimalVal(v); ok {
		s.addDecimalVal(dv)
	}
}

func (s *decimalSum) result() types.Decimal128 {
	if !s.any {
		return decimalZero()
	}

	p, ok := bson.ParseDecimal128FromBigInt(s.totalM, s.minExp)
	if !ok {
		return decimalZero()
	}

	h, l := p.GetBytes()

	return types.Decimal128{H: h, L: l}
}

// NumberSum incrementally sums numbers with the same type-promotion and
// Decimal128 semantics as SumNumbers, in O(1) space. Pre-decimal int and
// float64 values are folded via big.Int / float64 and converted to Decimal128
// only when a Decimal128 value first appears; all-int, all-float, all-decimal,
// and int+decimal groups match SumNumbers exactly, while a group mixing
// multiple float64 values with a Decimal128 may differ in the last ULP.
type NumberSum struct {
	intSum     *big.Int
	floatSum   float64
	hasInt     bool
	hasInt64   bool
	hasFloat64 bool
	sawDecimal bool
	dec        decimalSum
	count      int64
}

// NewNumberSum returns a zeroed incremental summer.
func NewNumberSum() *NumberSum {
	return &NumberSum{intSum: big.NewInt(0)}
}

// Count returns the number of numeric values folded so far.
func (s *NumberSum) Count() int64 { return s.count }

// Add folds one value; non-numeric values are ignored.
func (s *NumberSum) Add(v any) {
	switch n := v.(type) {
	case int32:
		s.count++
		if s.sawDecimal {
			s.dec.add(v)
			return
		}
		s.hasInt = true
		s.intSum.Add(s.intSum, big.NewInt(int64(n)))
	case int64:
		s.count++
		if s.sawDecimal {
			s.dec.add(v)
			return
		}
		s.hasInt = true
		s.hasInt64 = true
		s.intSum.Add(s.intSum, big.NewInt(n))
	case float64:
		s.count++
		if s.sawDecimal {
			s.dec.add(v)
			return
		}
		s.hasFloat64 = true
		s.floatSum += n
	case types.Decimal128:
		s.count++
		if !s.sawDecimal {
			s.promoteToDecimal()
		}
		s.dec.add(v)
	default:
		// ignore non-number
	}
}

// promoteToDecimal seeds the decimal accumulator from the running int and
// float sums when the first Decimal128 value arrives.
func (s *NumberSum) promoteToDecimal() {
	s.sawDecimal = true

	if s.hasInt {
		s.dec.addDecimalVal(decimalVal{new(big.Int).Set(s.intSum), 0})
	}

	if s.hasFloat64 {
		s.dec.add(s.floatSum)
	}
}

// Result returns the sum with SumNumbers' type rules. Empty input yields int32(0).
func (s *NumberSum) Result() any {
	if s.sawDecimal {
		return s.dec.result()
	}

	if s.hasFloat64 || !s.intSum.IsInt64() {
		intAsFloat, _ := new(big.Float).SetInt(s.intSum).Float64()

		return intAsFloat + s.floatSum
	}

	integer := s.intSum.Int64()

	if !s.hasInt64 && integer <= math.MaxInt32 && integer >= math.MinInt32 {
		return int32(integer)
	}

	return integer
}

// NumberAvg incrementally averages numbers with the same semantics as
// AvgNumbers, in O(1) space: float64 running sum for non-decimal input, and a
// Decimal128 running sum once any Decimal128 appears. Returns types.Null for
// zero numeric values.
type NumberAvg struct {
	floatSum   float64
	count      int64
	sawDecimal bool
	dec        decimalSum
}

// NewNumberAvg returns a zeroed incremental averager.
func NewNumberAvg() *NumberAvg { return &NumberAvg{} }

// Add folds one value; non-numeric values are ignored.
func (a *NumberAvg) Add(v any) {
	switch n := v.(type) {
	case int32:
		if a.sawDecimal {
			a.dec.add(v)
		} else {
			a.floatSum += float64(n)
		}
		a.count++
	case int64:
		if a.sawDecimal {
			a.dec.add(v)
		} else {
			a.floatSum += float64(n)
		}
		a.count++
	case float64:
		if a.sawDecimal {
			a.dec.add(v)
		} else {
			a.floatSum += n
		}
		a.count++
	case types.Decimal128:
		if !a.sawDecimal {
			a.sawDecimal = true
			if a.count > 0 {
				a.dec.add(a.floatSum)
			}
		}
		a.dec.add(v)
		a.count++
	default:
		// ignore non-number
	}
}

// Result returns the average, or types.Null when no numeric values were folded.
func (a *NumberAvg) Result() any {
	if a.count == 0 {
		return types.Null
	}

	if a.sawDecimal {
		return avgFromDecimalSum(a.dec.result(), a.count)
	}

	return a.floatSum / float64(a.count)
}
