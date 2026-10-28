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
	Asin
	Acos
	Atan
	Sinh
	Cosh
	Tanh
	Asinh
	Acosh
	Atanh
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
	switch fn {
	case Sinh:
		return fdlibmSinh(x)
	case Cosh:
		return fdlibmCosh(x)
	case Tanh:
		return fdlibmTanh(x)
	case Asinh:
		return fdlibmAsinh(x)
	case Acosh:
		return fdlibmAcosh(x)
	case Atanh:
		return fdlibmAtanh(x)
	}
	if math.IsNaN(x) || (math.IsInf(x, 0) && fn != Atan) {
		return math.NaN()
	}
	r, _ := roundedTrig(fn, new(big.Float).SetFloat64(x), binary64Precision).Float64()
	return r
}

// TrigAtan2Double returns atan2(y, x) correctly rounded to a double.
func TrigAtan2Double(y, x float64) float64 {
	if y == 0 || x == 0 || math.IsNaN(y) || math.IsNaN(x) || math.IsInf(y, 0) || math.IsInf(x, 0) {
		return math.Atan2(y, x)
	}
	r, _ := roundedAtan2(new(big.Float).SetFloat64(y), new(big.Float).SetFloat64(x), binary64Precision).Float64()
	return r
}

// roundedAtan2 returns atan2(y, x) correctly rounded to prec bits for finite
// nonzero y and x.
func roundedAtan2(y, x *big.Float, prec uint) *big.Float {
	work := uint(trigWorkPrecision + 64)
	q := new(big.Float).SetPrec(work).Quo(y, x)
	r := atanBig(q.Abs(q), work)
	if x.Signbit() {
		r.Sub(new(big.Float).SetPrec(work).Mul(halfPiFloat(work), big.NewFloat(2)), r)
	}
	if y.Signbit() {
		r.Neg(r)
	}
	return new(big.Float).SetPrec(prec).Set(r)
}

// roundedTrig returns fn(x) correctly rounded to prec bits.
func roundedTrig(fn TrigFunc, x *big.Float, prec uint) *big.Float {
	result := new(big.Float).SetPrec(prec)
	switch fn {
	case Asin:
		return result.Set(asinBig(x, trigWorkPrecision))
	case Acos:
		return result.Set(acosBig(x, trigWorkPrecision))
	case Atan:
		return result.Set(atanBig(x, trigWorkPrecision))
	case Sinh:
		return result.Set(sinhBig(x, trigWorkPrecision))
	case Cosh:
		return result.Set(coshBig(x, trigWorkPrecision))
	case Tanh:
		return result.Set(tanhBig(x, trigWorkPrecision))
	case Asinh:
		return result.Set(asinhBig(x, trigWorkPrecision))
	case Acosh:
		return result.Set(acoshBig(x, trigWorkPrecision))
	case Atanh:
		return result.Set(atanhBig(x, trigWorkPrecision))
	}
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

// atanBig evaluates atan(x) to about prec bits: arguments above 1 are
// inverted, then halved with atan(a) = 2*atan(a / (1 + sqrt(1 + a^2))) until
// the Taylor series converges quickly.
func atanBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	if x.IsInf() {
		r := halfPiFloat(work)
		if x.Signbit() {
			r.Neg(r)
		}
		return r
	}

	one := new(big.Float).SetPrec(work).SetInt64(1)
	a := new(big.Float).SetPrec(work).Abs(x)
	invert := a.Cmp(one) > 0
	if invert {
		a.Quo(one, a)
	}
	halvings := 0
	t := new(big.Float).SetPrec(work)
	for a.Sign() != 0 && a.MantExp(nil) > -12 {
		t.Mul(a, a)
		t.Add(t, one)
		t.Sqrt(t)
		t.Add(t, one)
		a.Quo(a, t)
		halvings++
	}

	a2 := new(big.Float).SetPrec(work).Mul(a, a)
	power := new(big.Float).SetPrec(work).Set(a)
	sum := new(big.Float).SetPrec(work).Set(a)
	limit := a.MantExp(nil) - int(work) - 8
	for k := int64(3); power.Sign() != 0 && power.MantExp(nil) > limit; k += 2 {
		power.Mul(power, a2)
		power.Neg(power)
		sum.Add(sum, t.Quo(power, new(big.Float).SetInt64(k)))
	}
	sum.SetMantExp(sum, halvings)

	if invert {
		sum.Sub(halfPiFloat(work), sum)
	}
	if x.Signbit() {
		sum.Neg(sum)
	}
	return sum
}

// asinBig evaluates asin(x) for |x| <= 1 as atan(x / sqrt(1 - x^2)).
func asinBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	t := new(big.Float).SetPrec(work).Mul(x, x)
	t.Sub(new(big.Float).SetPrec(work).SetInt64(1), t)
	if t.Sign() == 0 {
		r := halfPiFloat(work)
		if x.Signbit() {
			r.Neg(r)
		}
		return r
	}
	t.Sqrt(t)
	return atanBig(t.Quo(x, t), prec)
}

// acosBig evaluates acos(x) for |x| <= 1 as 2*atan(sqrt((1 - x) / (1 + x))),
// which keeps its accuracy near 1.
func acosBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	one := new(big.Float).SetPrec(work).SetInt64(1)
	den := new(big.Float).SetPrec(work).Add(one, x)
	if den.Sign() == 0 {
		return new(big.Float).SetPrec(work).Mul(halfPiFloat(work), big.NewFloat(2))
	}
	t := new(big.Float).SetPrec(work).Sub(one, x)
	t.Quo(t, den)
	r := atanBig(t.Sqrt(t), prec)
	return r.SetMantExp(r, 1)
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
	// Quad precision constants from the Intel library, by bit pattern.
	binary128HalfPi      = binary128FromBits(0x3fff921fb54442d1, 0x8469898cc51701b8)
	binary128Pi          = binary128FromBits(0x4000921fb54442d1, 0x8469898cc51701b8)
	binary128One         = binary128FromBits(0x3fff000000000000, 0)
	binary128SevenTenths = binary128FromBits(0x3ffe666666666666, 0x6666666666666666)
	binary128OneEm40     = binary128FromBits(0x3f7a16c262777579, 0xc58c46475896767b)

	// Decimal128 constants from the Intel library, by bit pattern.
	decimalPi             = types.Decimal128{H: 0x2ffe9ae4795796a7, L: 0xbabe5564e6f39f8f}
	decimalHalfPi         = types.Decimal128{H: 0x2ffe4d723cabcb53, L: 0xdd5f2ab27379cfc7}
	decimalHalfPiLow      = types.Decimal128{H: 0x2fbad9f8afb501d4, L: 0x0492b4138a162883}
	decimalQuarterPi      = types.Decimal128{H: 0x2ffe26b91e55e5a9, L: 0xeeaf955939bce7e4}
	decimalThreeQuarterPi = types.Decimal128{H: 0x2ffe742b5b01b0fd, L: 0xcc0ec00bad36b7ab}

	maxDecimalCoefficient = new(big.Int).Sub(new(big.Int).Exp(big.NewInt(10), big.NewInt(34), nil), big.NewInt(1))
	mask256               = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

// DecimalTrig returns fn(d) as MongoDB computes it. Sin, Cos and Tan take a
// finite d.
func DecimalTrig(fn TrigFunc, d types.Decimal128) types.Decimal128 {
	switch fn {
	case Asin, Acos, Atan:
		return decimalInverseTrig(fn, d)
	case Sinh, Cosh, Tanh:
		return decimalHyperbolic(fn, d)
	case Asinh, Acosh, Atanh:
		return decimalInverseHyperbolic(fn, d)
	default:
		return decimalCircularTrig(fn, d)
	}
}

func decimalCircularTrig(fn TrigFunc, d types.Decimal128) types.Decimal128 {
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
	return signedAPDToDecimal128(decimalFMAValue(x, y, z))
}

func decimalFMAValue(x, y, z *apd.Decimal) *apd.Decimal {
	product := new(apd.Decimal)
	_, _ = apd.BaseContext.WithPrecision(80).Mul(product, x, y)
	sum := new(apd.Decimal)
	_, _ = decimal128Context.Add(sum, product, z)
	return sum
}

func binary128FromBits(hi, lo uint64) *big.Float {
	m := new(big.Int).SetUint64(hi&(1<<48-1) | 1<<48)
	m.Lsh(m, 64).Or(m, new(big.Int).SetUint64(lo))
	f := new(big.Float).SetPrec(binary128Precision).SetInt(m)
	f.SetMantExp(f, int((hi>>48)&0x7fff)-16383-112)
	if hi>>63 == 1 {
		f.Neg(f)
	}
	return f
}

// apdToBinary128 converts x as bid128_to_binary128 does, overflowing to
// infinity past the binary128 range.
func apdToBinary128(x *apd.Decimal) *big.Float {
	f := new(big.Float).SetPrec(binary128Precision)
	switch {
	case x.Form == apd.Infinite:
		return f.SetInf(x.Negative)
	case x.IsZero():
		if x.Negative {
			return f.Neg(f)
		}
		return f
	}
	f.SetRat(decimalRat(x.Negative, x.Coeff.MathBigInt(), int(x.Exponent)))
	if f.MantExp(nil) > 16384 {
		return f.SetInf(x.Negative)
	}
	return f
}

func withDecimalSign(d types.Decimal128, negative bool) types.Decimal128 {
	if negative {
		d.H |= 1 << 63
	}
	return d
}

var decimalNaN = APDToDecimal128(&apd.Decimal{Form: apd.NaN})

// SignedDecimalToAPD is DecimalToAPD keeping the sign of a zero.
func SignedDecimalToAPD(d types.Decimal128) *apd.Decimal {
	x := DecimalToAPD(d)
	if x.Form == apd.Finite && x.IsZero() {
		x.Negative = d.H>>63 == 1
	}
	return x
}

// decimalQuo divides as IEEE 754 does: an exact quotient takes the exponent
// closest to exp(y) - exp(x), where apd keeps all 34 digits.
func decimalQuo(y, x *apd.Decimal) *apd.Decimal {
	z := new(apd.Decimal)
	cond, _ := decimal128Context.Quo(z, y, x)
	if cond.Inexact() || z.IsZero() {
		return z
	}
	ideal := y.Exponent - x.Exponent
	coeff := z.Coeff.MathBigInt()
	ten, rem := big.NewInt(10), new(big.Int)
	for z.Exponent < ideal {
		q, r := new(big.Int).QuoRem(coeff, ten, rem)
		if r.Sign() != 0 {
			break
		}
		coeff = q
		z.Exponent++
	}
	z.Coeff.SetMathBigInt(coeff)
	return z
}

// decimalInverseTrig follows bid128_asin, bid128_acos and bid128_atan, which
// pick their path by comparing the argument rounded to binary128.
func decimalInverseTrig(fn TrigFunc, d types.Decimal128) types.Decimal128 {
	x := SignedDecimalToAPD(d)
	if x.Form == apd.NaN {
		return decimalNaN
	}
	xd := apdToBinary128(x)
	abs := new(big.Float).Abs(xd)

	if abs.Cmp(binary128OneEm40) < 0 {
		switch fn {
		case Asin:
			return decimalFMA(x, apd.New(1, -40), x)
		case Acos:
			sum := new(apd.Decimal)
			_, _ = decimal128Context.Add(sum, DecimalToAPD(decimalHalfPi), DecimalToAPD(decimalHalfPiLow))
			return APDToDecimal128(sum)
		default:
			if x.IsZero() {
				return d
			}
			return decimalFMA(x, apd.New(-1, -40), x)
		}
	}
	if fn == Atan || abs.Cmp(binary128SevenTenths) <= 0 {
		return binary128ToDecimal(roundedTrig(fn, xd, binary128Precision))
	}
	if fn == Acos && xd.Cmp(binary128One) >= 0 {
		return types.Decimal128{}
	}

	// sqrt(1 - x^2) from a decimal fma, then the complementary function.
	t := apdToBinary128(decimalFMAValue(x, x, apd.New(-1, 0)))
	t.Neg(t).Sqrt(t)
	if fn == Asin {
		y := roundedTrig(Acos, t, binary128Precision)
		if xd.Signbit() {
			y.Neg(y)
		}
		return binary128ToDecimal(y)
	}
	y := roundedTrig(Asin, t, binary128Precision)
	if xd.Signbit() {
		y.Sub(binary128Pi, y)
	}
	return binary128ToDecimal(y)
}

// DecimalAtan2 returns atan2(y, x) as bid128_atan2 computes it.
func DecimalAtan2(y, x *apd.Decimal) types.Decimal128 {
	switch {
	case y.Form == apd.NaN:
		return decimalNaN
	case y.Form == apd.Infinite:
		switch {
		case x.Form == apd.Infinite && x.Negative:
			return withDecimalSign(decimalThreeQuarterPi, y.Negative)
		case x.Form == apd.Infinite:
			return withDecimalSign(decimalQuarterPi, y.Negative)
		case x.Form == apd.NaN:
			return decimalNaN
		default:
			return withDecimalSign(decimalHalfPi, y.Negative)
		}
	case x.Form == apd.NaN:
		return decimalNaN
	case y.IsZero() || x.Form == apd.Infinite:
		if x.Negative {
			return withDecimalSign(decimalPi, y.Negative)
		}
		return withDecimalSign(types.Decimal128{}, y.Negative)
	case x.IsZero():
		return withDecimalSign(decimalHalfPi, y.Negative)
	}

	z := decimalQuo(y, x)
	zabs := new(apd.Decimal).Abs(z)
	if zabs.Cmp(apd.New(1, 36)) > 0 {
		return withDecimalSign(decimalHalfPi, y.Negative)
	}
	if zabs.Cmp(apd.New(1, -36)) < 0 {
		if x.Negative {
			return withDecimalSign(decimalPi, y.Negative)
		}
		return signedAPDToDecimal128(z)
	}

	res := binary128ToDecimal(roundedTrig(Atan, apdToBinary128(zabs), binary128Precision))
	if x.Negative {
		diff := new(apd.Decimal)
		_, _ = decimal128Context.Sub(diff, DecimalToAPD(decimalPi), DecimalToAPD(res))
		res = APDToDecimal128(diff)
	}
	return withDecimalSign(res, y.Negative)
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
	if y.IsInf() {
		return APDToDecimal128(&apd.Decimal{Form: apd.Infinite, Negative: negative})
	}
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
