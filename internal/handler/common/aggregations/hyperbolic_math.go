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

// Hyperbolic functions that reproduce MongoDB's results exactly.
//
// glibc computes double sinh, cosh and tanh with fdlibm's formulas over expm1
// and exp, which are not correctly rounded, so those formulas are ported here
// (math.Expm1 is fdlibm's expm1; exp is correctly rounded, as glibc's nearly
// always is, see workspace-m8c.13). Decimal128 follows bid128_sinh,
// bid128_cosh and bid128_tanh.

var (
	decimalShifter  = types.Decimal128{H: 0x3040363bf3b1ceee, L: 0xbe00000000000000}
	decimalExp11000 = types.Decimal128{H: 0x5550558ada285f8b, L: 0xd43ede775707fd0a}

	binary128SixtyFour = binary128FromBits(0x4005000000000000, 0)
	binary128Eleven000 = binary128FromBits(0x400c57c000000000, 0)
)

const lnTwoFixedBits = 2048

// lnTwoFixed is floor(ln(2) * 2^lnTwoFixedBits), summing 1 / (k * 2^k).
var lnTwoFixed = sync.OnceValue(func() *big.Int {
	const guard = 64
	bits := uint(lnTwoFixedBits + guard)
	sum := new(big.Int)
	term := new(big.Int)
	for k := uint(1); k <= bits; k++ {
		term.Lsh(big.NewInt(1), bits-k)
		sum.Add(sum, term.Quo(term, big.NewInt(int64(k))))
	}
	return sum.Rsh(sum, guard)
})

func lnTwoFloat(prec uint) *big.Float {
	f := new(big.Float).SetPrec(prec).SetInt(lnTwoFixed())
	return f.SetMantExp(f, -lnTwoFixedBits)
}

// expBig evaluates e^x for finite x to about prec bits.
func expBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	if e := x.MantExp(nil); e > 0 {
		work += uint(e)
	}
	ln2 := lnTwoFloat(work)
	q := new(big.Float).SetPrec(work).Quo(x, ln2)
	if q.Sign() > 0 {
		q.Add(q, big.NewFloat(0.5))
	} else {
		q.Sub(q, big.NewFloat(0.5))
	}
	k, _ := q.Int(nil)
	r := new(big.Float).SetPrec(work).SetInt(k)
	r.Mul(r, ln2)
	r.Sub(new(big.Float).SetPrec(work).Set(x), r)

	const halvings = 10
	r.SetMantExp(r, -halvings)
	sum := new(big.Float).SetPrec(work).SetInt64(1)
	term := new(big.Float).SetPrec(work).SetInt64(1)
	for n := int64(1); term.Sign() != 0 && term.MantExp(nil) > -int(work)-8; n++ {
		term.Mul(term, r)
		term.Quo(term, new(big.Float).SetInt64(n))
		sum.Add(sum, term)
	}
	for range halvings {
		sum.Mul(sum, sum)
	}
	return sum.SetMantExp(sum, int(k.Int64()))
}

// sinhBig evaluates sinh(x) for finite x, by its series for |x| <= 1 to keep
// small results accurate.
func sinhBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	if x.Sign() == 0 {
		return new(big.Float).SetPrec(work).Set(x)
	}
	if new(big.Float).Abs(x).Cmp(big.NewFloat(1)) <= 0 {
		x2 := new(big.Float).SetPrec(work).Mul(x, x)
		term := new(big.Float).SetPrec(work).Set(x)
		sum := new(big.Float).SetPrec(work).Set(x)
		limit := x.MantExp(nil) - int(work) - 8
		for k := int64(2); term.Sign() != 0 && term.MantExp(nil) > limit; k += 2 {
			term.Mul(term, x2)
			term.Quo(term, new(big.Float).SetInt64(k*(k+1)))
			sum.Add(sum, term)
		}
		return sum
	}
	e := expBig(x, work)
	r := new(big.Float).SetPrec(work).Quo(big.NewFloat(1), e)
	r.Sub(e, r)
	return r.SetMantExp(r, -1)
}

func coshBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	e := expBig(x, work)
	r := new(big.Float).SetPrec(work).Quo(big.NewFloat(1), e)
	r.Add(e, r)
	return r.SetMantExp(r, -1)
}

// tanhBig evaluates tanh(x), which is +/-1 to well beyond binary128
// precision once |x| > 200.
func tanhBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	if x.IsInf() || new(big.Float).Abs(x).Cmp(big.NewFloat(200)) > 0 {
		one := new(big.Float).SetPrec(work).SetInt64(1)
		if x.Signbit() {
			one.Neg(one)
		}
		return one
	}
	r := sinhBig(x, prec)
	return r.Quo(r, coshBig(x, prec))
}

// expDouble is e^x correctly rounded, as glibc's exp nearly always is.
func expDouble(x float64) float64 {
	switch {
	case math.IsNaN(x) || math.IsInf(x, 1):
		return x
	case math.IsInf(x, -1) || x < -746:
		return 0
	case x > 710:
		return math.Inf(1)
	}
	r, _ := new(big.Float).SetPrec(binary64Precision).Set(expBig(new(big.Float).SetFloat64(x), trigWorkPrecision)).Float64()
	return r
}

func highWord(x float64) uint32 {
	return uint32(math.Float64bits(x) >> 32)
}

// fdlibmSinh is fdlibm's e_sinh.c, as glibc computes sinh.
func fdlibmSinh(x float64) float64 {
	ix := highWord(x) & 0x7fffffff
	if ix >= 0x7ff00000 {
		return x + x
	}
	h := 0.5
	if x < 0 {
		h = -h
	}
	ax := math.Abs(x)
	if ix < 0x40360000 {
		if ix < 0x3e300000 {
			return x
		}
		t := math.Expm1(ax)
		if ix < 0x3ff00000 {
			return h * (2.0*t - t*t/(t+1))
		}
		return h * (t + t/(t+1))
	}
	if ix < 0x40862e42 {
		return h * expDouble(ax)
	}
	if math.Float64bits(ax) <= 0x408633ce8fb9f87d {
		w := expDouble(0.5 * ax)
		t := h * w
		return t * w
	}
	return x * 1.0e307
}

// fdlibmCosh is fdlibm's e_cosh.c, as glibc computes cosh.
func fdlibmCosh(x float64) float64 {
	ix := highWord(x) & 0x7fffffff
	ax := math.Abs(x)
	if ix < 0x40360000 {
		if ix < 0x3fd62e43 {
			if ix < 0x3c800000 {
				return 1
			}
			t := math.Expm1(ax)
			w := 1 + t
			return 1 + (t*t)/(w+w)
		}
		t := expDouble(ax)
		return 0.5*t + 0.5/t
	}
	if ix < 0x40862e42 {
		return 0.5 * expDouble(ax)
	}
	if math.Float64bits(ax) <= 0x408633ce8fb9f87d {
		w := expDouble(0.5 * ax)
		t := 0.5 * w
		return t * w
	}
	if ix >= 0x7ff00000 {
		return x * x
	}
	return math.Inf(1)
}

// fdlibmTanh is fdlibm's s_tanh.c, as glibc computes tanh.
func fdlibmTanh(x float64) float64 {
	jx := highWord(x)
	ix := jx & 0x7fffffff
	if ix >= 0x7ff00000 {
		if jx>>31 == 0 {
			return 1/x + 1
		}
		return 1/x - 1
	}
	var z float64
	if ix < 0x40360000 {
		if x == 0 {
			return x
		}
		if ix < 0x3c800000 {
			return x * (1 + x)
		}
		ax := math.Abs(x)
		if ix >= 0x3ff00000 {
			t := math.Expm1(2 * ax)
			z = 1 - 2/(t+2)
		} else {
			t := math.Expm1(-2 * ax)
			z = -t / (t + 2)
		}
	} else {
		z = 1
	}
	if jx>>31 == 0 {
		return z
	}
	return -z
}

// quadRound rounds f to binary128, overflowing to infinity and underflowing
// through its subnormals.
func quadRound(f *big.Float) *big.Float {
	r := binary128Subnormal(new(big.Float).SetPrec(binary128Precision).Set(f))
	if !r.IsInf() && r.Sign() != 0 && r.MantExp(nil) > 16384 {
		r.SetInf(r.Signbit())
	}
	return r
}

func expQuad(x *big.Float) *big.Float {
	switch {
	case x.IsInf() && !x.Signbit():
		return new(big.Float).SetPrec(binary128Precision).SetInf(false)
	case x.IsInf() || x.Cmp(big.NewFloat(-11500)) < 0:
		return new(big.Float).SetPrec(binary128Precision)
	case x.Cmp(big.NewFloat(11400)) > 0:
		return new(big.Float).SetPrec(binary128Precision).SetInf(false)
	}
	return quadRound(expBig(x, trigWorkPrecision))
}

// mulQuad multiplies in binary128; ok is false for the NaN of infinity times zero.
func mulQuad(a, b *big.Float) (*big.Float, bool) {
	if (a.IsInf() && b.Sign() == 0) || (b.IsInf() && a.Sign() == 0) {
		return nil, false
	}
	return quadRound(new(big.Float).SetPrec(2*binary128Precision).Mul(a, b)), true
}

// apdToBinary128TwoPart converts x as bid128_to_binary128_2part does: the
// high part is x truncated to binary128, the low part the truncated remainder
// within the next 128 bits.
func apdToBinary128TwoPart(x *apd.Decimal) (*big.Float, *big.Float) {
	r := decimalRat(x.Negative, x.Coeff.MathBigInt(), int(x.Exponent))
	hi := new(big.Float).SetPrec(binary128Precision).SetMode(big.ToZero).SetRat(r)
	hi.SetMode(big.ToNearestEven)

	hiRat, _ := hi.Rat(nil)
	rem := new(big.Rat).Sub(r, hiRat)
	lsb := hi.MantExp(nil) - binary128Precision - 128
	scaled := new(big.Rat).Mul(rem, new(big.Rat).SetFrac(new(big.Int).Lsh(big.NewInt(1), uint(max(-lsb, 0))), new(big.Int).Lsh(big.NewInt(1), uint(max(lsb, 0)))))
	window := new(big.Int).Quo(scaled.Num(), scaled.Denom())

	lo := new(big.Float).SetPrec(binary128Precision).SetMode(big.ToZero).SetInt(window)
	lo.SetMantExp(lo, lsb).SetMode(big.ToNearestEven)
	return hi, lo
}

// decimalExpModerate is bid128_exp for 1 < x <= 64, where the binary128
// exponential cannot overflow.
func decimalExpModerate(x *apd.Decimal) *apd.Decimal {
	hi, lo := apdToBinary128TwoPart(x)
	rq := expQuad(hi)
	rt, _ := mulQuad(rq, lo)
	rq = quadRound(new(big.Float).SetPrec(2*binary128Precision).Add(rq, rt))
	return DecimalToAPD(binary128ToDecimal(rq))
}

// decimalHyperbolic follows bid128_sinh, bid128_cosh and bid128_tanh.
func decimalHyperbolic(fn TrigFunc, d types.Decimal128) types.Decimal128 {
	x := SignedDecimalToAPD(d)
	switch {
	case x.Form == apd.NaN:
		return decimalNaN
	case x.Form == apd.Infinite && fn == Sinh:
		return APDToDecimal128(x)
	case x.Form == apd.Infinite && fn == Cosh:
		return APDToDecimal128(&apd.Decimal{Form: apd.Infinite})
	}

	xd := apdToBinary128(x)
	abs := new(big.Float).Abs(xd)
	if fn == Tanh {
		if abs.Cmp(binary128OneEm40) < 0 {
			if x.IsZero() {
				return d
			}
			return decimalFMA(x, apd.New(-1, -40), x)
		}
		return binary128ToDecimal(roundedTrig(Tanh, xd, binary128Precision))
	}

	switch {
	case abs.Cmp(binary128OneEm40) <= 0:
		if fn == Sinh {
			return decimalFMA(x, apd.New(1, -40), x)
		}
		return decimalFMA(apd.New(1, 0), apd.New(1, 0), apd.New(1, -40))
	case abs.Cmp(binary128One) <= 0:
		return binary128ToDecimal(roundedTrig(fn, xd, binary128Precision))
	case abs.Cmp(binary128SixtyFour) <= 0:
		e := decimalExpModerate(new(apd.Decimal).Abs(x))
		inverse := decimalQuo(apd.New(1, 0), e)
		half := apd.New(5, -1)
		if fn == Sinh && !x.Negative {
			half.Negative = true
		}
		_, _ = decimal128Context.Mul(inverse, half, inverse)
		outer := apd.New(5, -1)
		if fn == Sinh && x.Negative {
			outer.Negative = true
		}
		return decimalFMA(e, outer, inverse)
	}

	ax := new(apd.Decimal).Abs(x)
	shifter := DecimalToAPD(decimalShifter)
	t, n, m := new(apd.Decimal), new(apd.Decimal), new(apd.Decimal)
	_, _ = decimal128Context.Add(t, ax, shifter)
	_, _ = decimal128Context.Sub(n, t, shifter)
	_, _ = decimal128Context.Sub(m, ax, n)
	nd, md := apdToBinary128(n), apdToBinary128(m)
	scaled := nd.Cmp(binary128Eleven000) > 0
	if scaled {
		nd = quadRound(new(big.Float).SetPrec(2*binary128Precision).Sub(nd, binary128Eleven000))
	}
	rd, ok := mulQuad(expQuad(nd), expQuad(md))
	if !ok {
		return decimalNaN
	}
	half := big.NewFloat(0.5)
	if fn == Sinh && xd.Signbit() {
		half.Neg(half)
	}
	rd.Mul(rd, half)
	res := binary128ToDecimal(rd)
	if !scaled {
		return res
	}
	product := new(apd.Decimal)
	_, _ = decimal128Context.Mul(product, DecimalToAPD(res), DecimalToAPD(decimalExp11000))
	return APDToDecimal128(product)
}
