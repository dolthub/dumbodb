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

// Decimal128ToInt64 returns d as an int64 when it is a finite integral value
// that fits; ok is false otherwise.
func Decimal128ToInt64(d types.Decimal128) (n int64, ok bool) {
	a, ok := decimalToAPD(d)
	if !ok {
		return 0, false
	}
	n, err := a.Int64()
	if err != nil {
		return 0, false
	}
	return n, true
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
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return decimalVal{}, false
		}

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
	if specialDecimal(sum) != nil {
		return sum
	}

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

// sumDecimal128 sums a slice of numbers using Decimal128 arithmetic.
// Called when at least one element is types.Decimal128.
func sumDecimal128(vs []any) types.Decimal128 {
	var s decimalSum
	for _, v := range vs {
		s.add(v)
	}

	return s.result()
}

// decimalSum incrementally accumulates Decimal128 values, producing the same
// total as sumDecimal128 over the same sequence. Integer-mantissa addition at a
// common minimum exponent is associative, so the incremental and batch results
// match exactly.
type decimalSum struct {
	totalM *big.Int
	minExp int
	any    bool

	// special is the NaN or infinity the total has become, which no finite
	// value can change.
	special *apd.Decimal
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
	if special := specialDecimal(v); special != nil {
		s.addSpecial(special)
		return
	}

	if dv, ok := toDecimalVal(v); ok {
		s.addDecimalVal(dv)
	}
}

// addSpecial follows IEEE 754: NaN absorbs everything, and infinities of
// opposite sign make NaN.
func (s *decimalSum) addSpecial(x *apd.Decimal) {
	switch {
	case s.special == nil:
		s.special = x
	case s.special.Form == apd.NaN:
	case x.Form == apd.NaN || x.Negative != s.special.Negative:
		s.special = &apd.Decimal{Form: apd.NaN}
	}
}

// specialDecimal returns v as a Decimal128 NaN or infinity, or nil when v
// is not one.
func specialDecimal(v any) *apd.Decimal {
	switch v := v.(type) {
	case float64:
		switch {
		case math.IsNaN(v):
			return &apd.Decimal{Form: apd.NaN}
		case math.IsInf(v, 0):
			return &apd.Decimal{Form: apd.Infinite, Negative: v < 0}
		}
	case types.Decimal128:
		if x := DecimalToAPD(v); x.Form != apd.Finite {
			return x
		}
	}

	return nil
}

func (s *decimalSum) result() types.Decimal128 {
	if s.special != nil {
		return APDToDecimal128(s.special)
	}

	if !s.any {
		return decimalZero()
	}

	// The exact total can need more than 34 digits; round it as Decimal128
	// arithmetic does.
	x := apd.NewWithBigInt(new(apd.BigInt).SetMathBigInt(s.totalM), int32(s.minExp))

	return APDToDecimal128(x)
}

// NumberSum incrementally sums numbers as mongod's $sum accumulator does:
// integers and doubles in a double-double, Decimal128 values by Decimal128
// addition, and the result typed by the widest input.
type NumberSum struct {
	nonDecimal doubleDoubleSum
	dec        types.Decimal128
	totalType  numberType
	count      int64
}

// numberType orders the numeric types as mongod widens them.
type numberType int

const (
	numberInt numberType = iota
	numberLong
	numberDouble
	numberDecimal
)

// NewNumberSum returns a zeroed incremental summer.
func NewNumberSum() *NumberSum {
	return &NumberSum{dec: decimalZero()}
}

// Count returns the number of numeric values folded so far.
func (s *NumberSum) Count() int64 { return s.count }

// Add folds one value; non-numeric values are ignored.
func (s *NumberSum) Add(v any) {
	switch n := v.(type) {
	case int32:
		s.nonDecimal.addDouble(float64(n))
	case int64:
		s.totalType = max(s.totalType, numberLong)
		s.nonDecimal.addLong(n)
	case float64:
		s.totalType = max(s.totalType, numberDouble)
		s.nonDecimal.addDouble(n)
	case types.Decimal128:
		s.totalType = numberDecimal
		s.dec = decimalAdd(s.dec, n)
	default:
		return
	}

	s.count++
}

// Result returns the sum. Empty input yields int32(0).
func (s *NumberSum) Result() any {
	switch s.totalType {
	case numberInt:
		if s.nonDecimal.fitsLong() {
			return intOrLong(s.nonDecimal.long())
		}
	case numberLong:
		if s.nonDecimal.fitsLong() {
			return s.nonDecimal.long()
		}
	case numberDecimal:
		return s.decimalTotal()
	}

	return s.nonDecimal.double()
}

func (s *NumberSum) decimalTotal() types.Decimal128 {
	return decimalAdd(s.dec, s.nonDecimal.decimal())
}

// roundedDecimal converts f to Decimal128, rounding to 34 digits.
func roundedDecimal(f float64) types.Decimal128 {
	var d decimalSum
	d.add(f)
	return d.result()
}

// NumberAvg incrementally averages numbers as mongod's $avg accumulator does.
// Returns types.Null for zero numeric values.
type NumberAvg struct {
	sum *NumberSum
}

// NewNumberAvg returns a zeroed incremental averager.
func NewNumberAvg() *NumberAvg { return &NumberAvg{sum: NewNumberSum()} }

// Add folds one value; non-numeric values are ignored.
func (a *NumberAvg) Add(v any) { a.sum.Add(v) }

// Result returns the average, or types.Null when no numeric values were folded.
func (a *NumberAvg) Result() any {
	if a.sum.count == 0 {
		return types.Null
	}

	if a.sum.totalType == numberDecimal {
		return avgFromDecimalSum(a.sum.decimalTotal(), a.sum.count)
	}

	return a.sum.nonDecimal.double() / float64(a.sum.count)
}
