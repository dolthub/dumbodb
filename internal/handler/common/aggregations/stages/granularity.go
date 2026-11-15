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

package stages

import (
	"math"
	"math/big"
	"math/bits"
	"slices"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// granularityRounder rounds non-negative numbers to the boundaries of a
// $bucketAuto granularity, as mongod's GranularityRounder does. roundUp
// returns the smallest boundary greater than a value and roundDown the
// largest one less than it; zero rounds to itself.
type granularityRounder interface {
	roundUp(v any) (any, error)
	roundDown(v any) (any, error)
}

// preferredNumberSeries are mongod's preferred number series over one decade.
// mongod's R40 omits 335 and its R80 holds every other value of the standard
// series.
var preferredNumberSeries = map[string][]float64{
	"R5": {
		10, 16, 25, 40, 63,
	},
	"R10": {
		100, 125, 160, 200, 250, 315, 400, 500, 630, 800,
	},
	"R20": {
		100, 112, 125, 140, 160, 180, 200, 224, 250, 280, 315, 355, 400, 450, 500, 560, 630, 710, 800, 900,
	},
	"R40": {
		100, 106, 112, 118, 125, 132, 140, 150, 160, 170, 180, 190, 200, 212, 224, 236, 250, 265, 280, 300, 315, 355,
		375, 400, 425, 450, 475, 500, 530, 560, 600, 630, 670, 710, 750, 800, 850, 900, 950,
	},
	"R80": {
		103, 109, 115, 122, 128, 136, 145, 155, 165, 175, 185, 195, 206, 218, 230, 243, 258, 272, 290, 307, 325, 345,
		365, 387, 412, 437, 462, 487, 515, 545, 575, 615, 650, 690, 730, 775, 825, 875, 925, 975,
	},
	"1-2-5": {
		10, 20, 50,
	},
	"E6": {
		10, 15, 22, 33, 47, 68,
	},
	"E12": {
		10, 12, 15, 18, 22, 27, 33, 39, 47, 56, 68, 82,
	},
	"E24": {
		10, 11, 12, 13, 15, 16, 18, 20, 22, 24, 27, 30, 33, 36, 39, 43, 47, 51, 56, 62, 68, 75, 82, 91,
	},
	"E48": {
		100, 105, 110, 115, 121, 127, 133, 140, 147, 154, 162, 169, 178, 187, 196, 205, 215, 226, 237, 249, 261, 274,
		287, 301, 316, 332, 348, 365, 383, 402, 422, 442, 464, 487, 511, 536, 562, 590, 619, 649, 681, 715, 750, 787,
		825, 866, 909, 953,
	},
	"E96": {
		100, 102, 105, 107, 110, 113, 115, 118, 121, 124, 127, 130, 133, 137, 140, 143, 147, 150, 154, 158, 162, 165,
		169, 174, 178, 182, 187, 191, 196, 200, 205, 210, 215, 221, 226, 232, 237, 243, 249, 255, 261, 267, 274, 280,
		287, 294, 301, 309, 316, 324, 332, 340, 348, 357, 365, 374, 383, 392, 402, 412, 422, 432, 442, 453, 464, 475,
		487, 499, 511, 523, 536, 549, 562, 576, 590, 604, 619, 634, 649, 665, 681, 698, 715, 732, 750, 768, 787, 806,
		825, 845, 866, 887, 909, 931, 953, 976,
	},
	"E192": {
		100, 101, 102, 104, 105, 106, 107, 109, 110, 111, 113, 114, 115, 117, 118, 120, 121, 123, 124, 126, 127, 129,
		130, 132, 133, 135, 137, 138, 140, 142, 143, 145, 147, 149, 150, 152, 154, 156, 158, 160, 162, 164, 165, 167,
		169, 172, 174, 176, 178, 180, 182, 184, 187, 189, 191, 193, 196, 198, 200, 203, 205, 208, 210, 213, 215, 218,
		221, 223, 226, 229, 232, 234, 237, 240, 243, 246, 249, 252, 255, 258, 261, 264, 267, 271, 274, 277, 280, 284,
		287, 291, 294, 298, 301, 305, 309, 312, 316, 320, 324, 328, 332, 336, 340, 344, 348, 352, 357, 361, 365, 370,
		374, 379, 383, 388, 392, 397, 402, 407, 412, 417, 422, 427, 432, 437, 442, 448, 453, 459, 464, 470, 475, 481,
		487, 493, 499, 505, 511, 517, 523, 530, 536, 542, 549, 556, 562, 569, 576, 583, 590, 597, 604, 612, 619, 626,
		634, 642, 649, 657, 665, 673, 681, 690, 698, 706, 715, 723, 732, 741, 750, 759, 768, 777, 787, 796, 806, 816,
		825, 835, 845, 856, 866, 876, 887, 898, 909, 920, 931, 942, 953, 965, 976, 988,
	},
}

// newGranularityRounder returns the rounder for name, or false when mongod
// has no such granularity.
func newGranularityRounder(name string) (granularityRounder, bool) {
	if name == "POWERSOF2" {
		return powersOfTwoRounder{}, true
	}
	series, ok := preferredNumberSeries[name]
	if !ok {
		return nil, false
	}
	return &preferredNumberRounder{series: series}, true
}

type preferredNumberRounder struct {
	series []float64
}

func (r *preferredNumberRounder) roundUp(v any) (any, error) {
	return r.round(v, true), nil
}

func (r *preferredNumberRounder) roundDown(v any) (any, error) {
	return r.round(v, false), nil
}

// checkFiniteForRounding is mongod's check that the powers-of-two rounder
// rounds only finite numbers.
func checkFiniteForRounding(v any) error {
	infinite := false
	switch n := v.(type) {
	case float64:
		infinite = math.IsInf(n, 0)
	case types.Decimal128:
		infinite = aggregations.DecimalToAPD(n).Form == apd.Infinite
	}
	if infinite {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrGranularityNotFinite,
			"A granularity rounder can only round finite numbers", "$bucketAuto (stage)")
	}
	return nil
}

// round follows mongod's GranularityRounderPreferredNumbers: it scales the
// series by powers of ten until it spans v, in v's arithmetic (decimal for a
// Decimal128, double otherwise), and takes the neighbouring scaled value.
func (r *preferredNumberRounder) round(v any, up bool) any {
	if d, ok := v.(types.Decimal128); ok {
		return r.roundDecimal(v, aggregations.DecimalToAPD(d), up)
	}
	number, _ := toFloat64(v)
	if number == 0 || math.IsInf(number, 0) {
		return v
	}
	front, back := r.series[0], r.series[len(r.series)-1]
	m := 1.0
	if up {
		for number >= back*m {
			m *= 10
		}
		for number < front*m {
			previousMin := front * m
			m /= 10
			if number >= back*m {
				return previousMin
			}
		}
		idx := slices.IndexFunc(r.series, func(s float64) bool { return number < s*m })
		return r.series[idx] * m
	}
	for number <= front*m {
		m /= 10
	}
	if m == 0 {
		return 0.0
	}
	for number > back*m {
		previousMax := back * m
		m *= 10
		if number <= front*m {
			return previousMax
		}
	}
	idx := slices.IndexFunc(r.series, func(s float64) bool { return s*m >= number })
	return r.series[idx-1] * m
}

func (r *preferredNumberRounder) roundDecimal(v any, number *apd.Decimal, up bool) any {
	if f := decimalAsFloat(number); f == 0 || math.IsInf(f, 0) {
		return v
	}
	series := make([]*apd.Decimal, len(r.series))
	for i, s := range r.series {
		series[i] = aggregations.DoubleToAPD(s)
	}
	front, back := series[0], series[len(series)-1]
	m := apd.New(1, 0)
	scaled := func(s *apd.Decimal) *apd.Decimal { return aggregations.DecimalMultiply(s, m) }
	result := func(s *apd.Decimal) any { return aggregations.APDToDecimal128(scaled(s)) }
	if up {
		for number.Cmp(scaled(back)) >= 0 {
			m = decimalTimesTen(m)
		}
		for number.Cmp(scaled(front)) < 0 {
			previousMin := result(front)
			m = decimalDivTen(m)
			if number.Cmp(scaled(back)) >= 0 {
				return previousMin
			}
		}
		idx := slices.IndexFunc(series, func(s *apd.Decimal) bool { return number.Cmp(scaled(s)) < 0 })
		return result(series[idx])
	}
	for number.Cmp(scaled(front)) <= 0 {
		m = decimalDivTen(m)
	}
	for number.Cmp(scaled(back)) > 0 {
		previousMax := result(back)
		m = decimalTimesTen(m)
		if number.Cmp(scaled(front)) <= 0 {
			return previousMax
		}
	}
	idx := slices.IndexFunc(series, func(s *apd.Decimal) bool { return scaled(s).Cmp(number) >= 0 })
	return result(series[idx-1])
}

// decimalTimesTen and decimalDivTen are Decimal128 multiplication and division
// by 10, which are exact and keep IEEE 754's preferred exponent.
func decimalTimesTen(m *apd.Decimal) *apd.Decimal {
	return aggregations.DecimalMultiply(m, apd.New(10, 0))
}

func decimalDivTen(m *apd.Decimal) *apd.Decimal {
	coeff := m.Coeff.MathBigInt()
	q, rem := new(big.Int).QuoRem(coeff, big.NewInt(10), new(big.Int))
	res := new(apd.Decimal)
	if rem.Sign() == 0 {
		res.Coeff.SetMathBigInt(q)
		res.Exponent = m.Exponent
	} else {
		res.Coeff.SetMathBigInt(coeff)
		res.Exponent = m.Exponent - 1
	}
	return res
}

// powersOfTwoRounder rounds to powers of two, computed with $pow so the result
// has the type mongod gives it.
type powersOfTwoRounder struct{}

func (powersOfTwoRounder) roundUp(v any) (any, error) {
	return powersOfTwo(v, true)
}

func (powersOfTwoRounder) roundDown(v any) (any, error) {
	return powersOfTwo(v, false)
}

func powersOfTwo(v any, up bool) (any, error) {
	if err := checkFiniteForRounding(v); err != nil {
		return nil, err
	}
	var exp any
	switch n := v.(type) {
	case int32, int64:
		i, _ := n.(int64)
		if i32, ok := n.(int32); ok {
			i = int64(i32)
		}
		x := uint64(i)
		if x == 0 {
			return v, nil
		}
		floorLog := bits.Len64(x) - 1
		switch {
		case up:
			exp = int32(floorLog + 1)
		case x&(x-1) == 0:
			exp = int32(floorLog - 1)
		default:
			exp = int32(floorLog)
		}
	case float64:
		if n == 0 {
			return v, nil
		}
		exp = int32(powersOfTwoExponent(n, up))
	case types.Decimal128:
		f := decimalAsFloat(aggregations.DecimalToAPD(n))
		if math.IsInf(f, 0) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrGranularityNotFinite,
				"A granularity rounder can only round finite numbers", "$bucketAuto (stage)")
		}
		if f == 0 {
			return v, nil
		}
		exp = aggregations.APDToDecimal128(apd.New(int64(powersOfTwoExponent(f, up)), 0))
	default:
		return v, nil
	}
	res, _, err := operators.EvalOperand(must.NotFail(types.NewDocument("$pow", must.NotFail(types.NewArray(int32(2), exp)))), types.MakeDocument(0))
	return res, err
}

func powersOfTwoExponent(f float64, up bool) int {
	if up {
		return int(math.Floor(math.Log2(f))) + 1
	}
	return int(math.Ceil(math.Log2(f))) - 1
}
