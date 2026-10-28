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
	"math/big"
	"sync"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/types"
)

// Trigonometric functions that reproduce MongoDB's results exactly.
//
// Doubles are correctly rounded from a high precision evaluation, which is
// what glibc's libm returns. Decimal128 values follow the Intel decimal
// floating point library MongoDB links (bid128_sin.c and friends): small
// arguments take shortcuts, others are reduced modulo pi/2 in fixed point,
// evaluated as IEEE binary128 and converted back with binary128_to_bid128.

// TrigFunc selects a trigonometric function.
type TrigFunc int

// Trigonometric functions.
const (
	Sin TrigFunc = iota
	Cos
	Tan
)

const (
	binary64Precision  = 53
	binary128Precision = 113
	trigWorkPrecision  = 320

	// piFixedBits covers reducing the largest Decimal128 (10^6145) modulo
	// pi/2 with 256 fraction bits to spare.
	piFixedBits = 21504
)

// piFixed is floor(pi * 2^piFixedBits).
var piFixed = sync.OnceValue(func() *big.Int {
	const guard = 64
	bits := uint(piFixedBits + guard)
	pi := new(big.Int).Lsh(arctanInverse(5, bits), 4)
	pi.Sub(pi, new(big.Int).Lsh(arctanInverse(239, bits), 2))
	return pi.Rsh(pi, guard)
})

// arctanInverse returns atan(1/n) scaled by 2^bits.
func arctanInverse(n int64, bits uint) *big.Int {
	term := new(big.Int).Lsh(big.NewInt(1), bits)
	term.Quo(term, big.NewInt(n))
	sum := new(big.Int).Set(term)
	nn := big.NewInt(n * n)
	t := new(big.Int)
	for k := int64(1); term.Sign() != 0; k++ {
		term.Quo(term, nn)
		t.Quo(term, big.NewInt(2*k+1))
		if k%2 == 1 {
			sum.Sub(sum, t)
		} else {
			sum.Add(sum, t)
		}
	}
	return sum
}

func halfPiFloat(prec uint) *big.Float {
	f := new(big.Float).SetPrec(prec).SetInt(piFixed())
	return f.SetMantExp(f, -piFixedBits-1)
}

// TrigDouble returns fn(x) correctly rounded to a double.
func TrigDouble(fn TrigFunc, x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return math.NaN()
	}
	r, _ := roundedTrig(fn, new(big.Float).SetFloat64(x), binary64Precision).Float64()
	return r
}

// roundedTrig returns fn(x) correctly rounded to prec bits.
func roundedTrig(fn TrigFunc, x *big.Float, prec uint) *big.Float {
	result := new(big.Float).SetPrec(prec)
	if x.Sign() == 0 {
		if fn == Cos {
			return result.SetInt64(1)
		}
		return result.Set(x)
	}

	work := uint(trigWorkPrecision)
	if e := x.MantExp(nil); e > 0 {
		work += uint(e)
	}
	halfPi := halfPiFloat(work + 64)
	q := new(big.Float).SetPrec(work).Quo(x, halfPi)
	if q.Sign() > 0 {
		q.Add(q, big.NewFloat(0.5))
	} else {
		q.Sub(q, big.NewFloat(0.5))
	}
	n, _ := q.Int(nil)
	r := new(big.Float).SetPrec(work + 64).SetInt(n)
	r.Mul(r, halfPi)
	r.Sub(new(big.Float).SetPrec(work+64).Set(x), r)

	s, c := sinCosSeries(r, trigWorkPrecision)
	quadrant := new(big.Int).Mod(n, big.NewInt(4)).Int64()
	return result.Set(quadrantTrig(fn, quadrant, s, c))
}

// quadrantTrig returns fn(r + quadrant*pi/2) given sin(r) and cos(r).
func quadrantTrig(fn TrigFunc, quadrant int64, s, c *big.Float) *big.Float {
	neg := func(f *big.Float) *big.Float { return new(big.Float).Neg(f) }
	switch fn {
	case Sin:
		return [4]*big.Float{s, c, neg(s), neg(c)}[quadrant]
	case Cos:
		return [4]*big.Float{c, neg(s), neg(c), s}[quadrant]
	default:
		if quadrant%2 == 0 {
			return new(big.Float).SetPrec(s.Prec()).Quo(s, c)
		}
		return new(big.Float).SetPrec(s.Prec()).Quo(neg(c), s)
	}
}

// sinCosSeries evaluates the Taylor series for |r| <= pi/4.
func sinCosSeries(r *big.Float, prec uint) (*big.Float, *big.Float) {
	x := new(big.Float).SetPrec(prec).Set(r)
	x2 := new(big.Float).SetPrec(prec).Mul(x, x)
	sum := func(term *big.Float, first int64) *big.Float {
		total := new(big.Float).SetPrec(prec).Set(term)
		limit := total.MantExp(nil) - int(prec) - 8
		for k := first; term.Sign() != 0 && term.MantExp(nil) > limit; k += 2 {
			term.Mul(term, x2)
			term.Quo(term, new(big.Float).SetInt64(-k*(k+1)))
			total.Add(total, term)
		}
		return total
	}
	return sum(new(big.Float).SetPrec(prec).Set(x), 2), sum(new(big.Float).SetPrec(prec).SetInt64(1), 1)
}

var (
	// binary128HalfPi is the quad precision pi/2 constant c_pi_ov_2.
	binary128HalfPi = func() *big.Float {
		m, _ := new(big.Int).SetString("1921fb54442d18469898cc51701b8", 16)
		f := new(big.Float).SetPrec(binary128Precision).SetInt(m)
		return f.SetMantExp(f, -112)
	}()

	maxDecimalCoefficient = new(big.Int).Sub(new(big.Int).Exp(big.NewInt(10), big.NewInt(34), nil), big.NewInt(1))
	mask256               = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

// DecimalTrig returns fn(d) as MongoDB computes it for a finite Decimal128.
func DecimalTrig(fn TrigFunc, d types.Decimal128) types.Decimal128 {
	negative := d.H>>63 == 1
	coeff := new(big.Int)
	exp := 0
	if (d.H>>61)&3 != 3 {
		exp = int((d.H>>49)&(1<<14-1)) - 6176
		coeff.SetUint64(d.H & (1<<49 - 1))
		coeff.Lsh(coeff, 64).Or(coeff, new(big.Int).SetUint64(d.L))
		if coeff.Cmp(maxDecimalCoefficient) > 0 {
			coeff.SetInt64(0)
		}
	}

	pathExp := exp
	if coeff.Sign() == 0 {
		if fn == Sin {
			return d
		}
		pathExp = -53
	}

	if pathExp < -35 {
		if pathExp >= -52 {
			x := new(big.Float).SetPrec(binary128Precision).SetRat(decimalRat(negative, coeff, exp))
			return binary128ToDecimal(roundedTrig(fn, x, binary128Precision))
		}
		x := &apd.Decimal{Negative: negative, Exponent: int32(exp)}
		x.Coeff.SetMathBigInt(coeff)
		switch fn {
		case Sin:
			return decimalFMA(x, apd.New(-1, -40), x)
		case Cos:
			return decimalFMA(apd.New(1, 0), apd.New(1, 0), apd.New(1, -40))
		default:
			return decimalFMA(x, apd.New(1, -40), x)
		}
	}

	fraction := reducedFraction(coeff, exp)
	quadrant := new(big.Int).Rsh(fraction, 254).Uint64()
	g := new(big.Int).Lsh(fraction, 2)
	g.And(g, mask256)
	fractionNegative := negative
	if g.Bit(255) == 1 {
		quadrant = (quadrant + 1) & 3
		g.Sub(mask256, g)
		fractionNegative = !negative
	}
	if negative {
		quadrant = -quadrant & 3
	}

	xd := new(big.Float).SetPrec(binary128Precision).SetMode(big.ToZero).SetInt(g)
	xd.SetMantExp(xd, -256).SetMode(big.ToNearestEven)
	if fractionNegative {
		xd.Neg(xd)
	}
	xd.Mul(xd, binary128HalfPi)

	var yd *big.Float
	switch fn {
	case Tan:
		yd = roundedTrig(Tan, xd, binary128Precision)
		if quadrant%2 == 1 {
			yd = new(big.Float).SetPrec(binary128Precision).Quo(big.NewFloat(-1), yd)
		}
	default:
		base := fn
		if quadrant%2 == 1 {
			base = Sin + Cos - fn
		}
		yd = roundedTrig(base, xd, binary128Precision)
		if (fn == Sin && quadrant >= 2) || (fn == Cos && (quadrant == 1 || quadrant == 2)) {
			yd.Neg(yd)
		}
	}
	return binary128ToDecimal(yd)
}

// reducedFraction returns floor(frac(coeff * 10^exp / (2*pi)) * 2^256), the
// fixed point value bid128_sin's table multiplication produces: its top two
// bits are the quadrant and the rest the fraction of a quarter turn.
func reducedFraction(coeff *big.Int, exp int) *big.Int {
	n := new(big.Int).Set(coeff)
	if exp > 0 {
		n.Mul(n, pow10(exp))
	}
	bits := uint(n.BitLen() + 256 + 192)
	if exp < 0 {
		bits += uint(-4 * exp)
	}
	invTwoPi := new(big.Int).Lsh(big.NewInt(1), bits+piFixedBits)
	invTwoPi.Quo(invTwoPi, new(big.Int).Lsh(piFixed(), 1))
	n.Mul(n, invTwoPi)
	if exp < 0 {
		n.Quo(n, pow10(-exp))
	}
	n.Rsh(n, bits-256)
	return n.And(n, mask256)
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

func decimalRat(negative bool, coeff *big.Int, exp int) *big.Rat {
	r := new(big.Rat)
	if exp >= 0 {
		r.SetInt(new(big.Int).Mul(coeff, pow10(exp)))
	} else {
		r.SetFrac(coeff, pow10(-exp))
	}
	if negative {
		r.Neg(r)
	}
	return r
}

// decimalFMA returns x*y+z rounded once to Decimal128.
func decimalFMA(x, y, z *apd.Decimal) types.Decimal128 {
	product := new(apd.Decimal)
	_, _ = apd.BaseContext.WithPrecision(80).Mul(product, x, y)
	sum := new(apd.Decimal)
	_, _ = decimal128Context.Add(sum, product, z)
	return signedAPDToDecimal128(sum)
}

// signedAPDToDecimal128 is APDToDecimal128 keeping the sign of a zero.
func signedAPDToDecimal128(x *apd.Decimal) types.Decimal128 {
	d := APDToDecimal128(x)
	if x.Form == apd.Finite && x.Negative {
		d.H |= 1 << 63
	}
	return d
}

// binary128ToDecimal converts y as binary128_to_bid128 does: zero and exact
// values that fit in 34 digits keep their exact form (integers at exponent 0,
// fractions at the exponent of their last binary digit), anything else is
// rounded to all 34 digits. Every binary fraction has an exact decimal form,
// so building it and rounding once gives both.
func binary128ToDecimal(y *big.Float) types.Decimal128 {
	negative := y.Signbit()
	if y.Sign() == 0 {
		return signedAPDToDecimal128(&apd.Decimal{Negative: negative})
	}

	mant := new(big.Float).Abs(y)
	exp2 := mant.MantExp(mant) - binary128Precision
	mant.SetMantExp(mant, binary128Precision)
	m, _ := mant.Int(nil)
	tz := m.TrailingZeroBits()
	m.Rsh(m, tz)
	exp2 += int(tz)

	x := &apd.Decimal{Negative: negative}
	if exp2 >= 0 {
		x.Coeff.SetMathBigInt(m.Lsh(m, uint(exp2)))
	} else {
		x.Coeff.SetMathBigInt(m.Mul(m, new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(-exp2)), nil)))
		x.Exponent = int32(exp2)
	}
	return APDToDecimal128(x)
}
