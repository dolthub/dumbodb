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

package aggregations

import (
	"math"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/types"
)

// doubleDoubleSum is mongod's DoubleDoubleSummation: sum is the double
// nearest the total and addend the remainder; special is a plain running sum
// that stands in once sum becomes NaN.
type doubleDoubleSum struct {
	sum, addend, special float64
}

func (s *doubleDoubleSum) addDouble(x float64) {
	s.special += x
	x, s.addend = fast2Sum(x, s.addend)
	s.sum, x = twoSum(s.sum, x)
	s.addend += x
}

// addLong splits x into two doubles so the sum stays exact.
func (s *doubleDoubleSum) addLong(x int64) {
	high := x / (1 << 32) * (1 << 32)
	s.addDouble(float64(x - high))
	s.addDouble(float64(high))
}

func (s *doubleDoubleSum) double() float64 {
	if math.IsNaN(s.sum) {
		return s.special
	}
	return s.sum
}

func (s *doubleDoubleSum) decimal() types.Decimal128 {
	if math.IsNaN(s.sum) || math.IsInf(s.sum, 0) {
		return roundedDecimal(s.special)
	}
	return decimalAdd(roundedDecimal(s.sum), roundedDecimal(s.addend))
}

func (s *doubleDoubleSum) fitsLong() bool {
	switch {
	case s.sum > math.MinInt64 && s.sum < math.MaxInt64:
		return true
	case s.sum == math.MaxInt64:
		return s.addend < -0.5
	case s.sum == math.MinInt64:
		return s.addend >= -0.5
	}
	return false
}

// long rounds the total half away from zero; callers check fitsLong first.
func (s *doubleDoubleSum) long() int64 {
	if s.sum == math.MaxInt64 {
		return int64(math.Round(s.addend)) - math.MinInt64
	}
	sum := int64(math.Round(s.sum))
	return sum + int64(math.Round((s.sum-float64(sum))+s.addend))
}

func intOrLong(n int64) any {
	if n >= math.MinInt32 && n <= math.MaxInt32 {
		return int32(n)
	}
	return n
}

// fast2Sum assumes |b| <= |a|.
func fast2Sum(a, b float64) (float64, float64) {
	s := a + b
	z := s - a
	return s, b - z
}

func twoSum(a, b float64) (float64, float64) {
	s := a + b
	aPrime := s - b
	bPrime := s - aPrime
	return s, (a - aPrime) + (b - bPrime)
}

// decimalAdd is Decimal128 addition, rounding to 34 digits half to even.
func decimalAdd(a, b types.Decimal128) types.Decimal128 {
	x, y := SignedDecimalToAPD(a), SignedDecimalToAPD(b)
	if x.Form != apd.Finite || y.Form != apd.Finite {
		if special := addSpecials(x, y); special != nil {
			return APDToDecimal128(special)
		}
	}

	var out apd.Decimal
	if _, err := decimal128Context.Add(&out, x, y); err != nil {
		return APDToDecimal128(&apd.Decimal{Form: apd.NaN})
	}
	return APDToDecimal128(&out)
}

// addSpecials returns the NaN or infinity a sum involving one is.
func addSpecials(x, y *apd.Decimal) *apd.Decimal {
	var s decimalSum
	for _, d := range []*apd.Decimal{x, y} {
		if d.Form != apd.Finite {
			s.addSpecial(d)
		}
	}
	return s.special
}
