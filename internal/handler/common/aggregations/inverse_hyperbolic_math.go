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

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/types"
)

// Inverse hyperbolic functions that reproduce MongoDB's results exactly.
//
// glibc computes double asinh, acosh and atanh with fdlibm's formulas over
// log1p, log and sqrt, ported here (math.Log1p is fdlibm's log1p; log is
// correctly rounded, as glibc's nearly always is, see workspace-m8c.13).
// Decimal128 follows
// bid128_asinh, bid128_acosh and bid128_atanh.

var (
	binary128Ln10 = binary128FromBits(0x400026bb1bbb5551, 0x582dd4adac5705a6)

	decimalAcoshNearOne = apd.New(103125, -5)
)

// logBig evaluates ln(x) for finite x > 0 to about prec bits, as
// k*ln(2) + 2*atanh((m - 1) / (m + 1)) with sqrt(1/2) <= m < sqrt(2).
func logBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	m := new(big.Float).SetPrec(work)
	k := x.MantExp(m)
	if m.Cmp(big.NewFloat(math.Sqrt2/2)) < 0 {
		m.SetMantExp(m, 1)
		k--
	}
	one := new(big.Float).SetPrec(work).SetInt64(1)
	z := new(big.Float).SetPrec(work).Sub(m, one)
	z.Quo(z, new(big.Float).SetPrec(work).Add(m, one))
	z2 := new(big.Float).SetPrec(work).Mul(z, z)
	power := new(big.Float).SetPrec(work).Set(z)
	sum := new(big.Float).SetPrec(work).Set(z)
	term := new(big.Float).SetPrec(work)
	limit := z.MantExp(nil) - int(work) - 8
	for n := int64(3); power.Sign() != 0 && power.MantExp(nil) > limit; n += 2 {
		power.Mul(power, z2)
		sum.Add(sum, term.Quo(power, new(big.Float).SetInt64(n)))
	}
	sum.SetMantExp(sum, 1)
	return sum.Add(sum, new(big.Float).SetPrec(work).Mul(lnTwoFloat(work), new(big.Float).SetInt64(int64(k))))
}

// log1pBig evaluates ln(1 + x) for finite x > -1, by its series for tiny x.
func log1pBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	if x.Sign() == 0 {
		return new(big.Float).SetPrec(work).Set(x)
	}
	if x.MantExp(nil) < -20 {
		power := new(big.Float).SetPrec(work).Set(x)
		sum := new(big.Float).SetPrec(work).Set(x)
		term := new(big.Float).SetPrec(work)
		limit := x.MantExp(nil) - int(work) - 8
		for n := int64(2); power.Sign() != 0 && power.MantExp(nil) > limit; n++ {
			power.Mul(power, x)
			power.Neg(power)
			sum.Add(sum, term.Quo(power, new(big.Float).SetInt64(n)))
		}
		return sum
	}
	sum := new(big.Float).SetPrec(work + uint(max(-x.MantExp(nil), 0))).SetInt64(1)
	return logBig(sum.Add(sum, x), prec)
}

// asinhBig evaluates asinh(x) as log1p(|x| + x^2 / (1 + sqrt(1 + x^2))).
func asinhBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	if x.IsInf() {
		return new(big.Float).SetPrec(work).Set(x)
	}
	a := new(big.Float).SetPrec(work).Abs(x)
	t := new(big.Float).SetPrec(work).Mul(a, a)
	s := new(big.Float).SetPrec(work).Add(t, big.NewFloat(1))
	s.Sqrt(s)
	s.Add(s, big.NewFloat(1))
	t.Quo(t, s)
	r := log1pBig(t.Add(t, a), prec)
	if x.Signbit() {
		r.Neg(r)
	}
	return r
}

// acoshBig evaluates acosh(x) for x >= 1 as log1p(t + sqrt(2t + t^2)) with
// t = x - 1.
func acoshBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	if x.IsInf() {
		return new(big.Float).SetPrec(work).Set(x)
	}
	t := new(big.Float).SetPrec(work).Sub(x, big.NewFloat(1))
	s := new(big.Float).SetPrec(work).Add(t, big.NewFloat(2))
	s.Mul(s, t)
	s.Sqrt(s)
	return log1pBig(s.Add(s, t), prec)
}

// atanhBig evaluates atanh(x) for |x| < 1 as log1p(2|x| / (1 - |x|)) / 2.
func atanhBig(x *big.Float, prec uint) *big.Float {
	work := prec + 64
	a := new(big.Float).SetPrec(work).Abs(x)
	t := new(big.Float).SetPrec(work).Sub(big.NewFloat(1), a)
	t.Quo(a, t)
	r := log1pBig(t.SetMantExp(t, 1), prec)
	r.SetMantExp(r, -1)
	if x.Signbit() {
		r.Neg(r)
	}
	return r
}

// logDouble is ln(x) correctly rounded, as glibc's log nearly always is.
func logDouble(x float64) float64 {
	r, _ := new(big.Float).SetPrec(binary64Precision).Set(logBig(new(big.Float).SetFloat64(x), trigWorkPrecision)).Float64()
	return r
}

const fdlibmLn2 = 6.93147180559945286227e-01

// fdlibmAsinh is fdlibm's s_asinh.c, as glibc computes asinh.
func fdlibmAsinh(x float64) float64 {
	hx := highWord(x)
	ix := hx & 0x7fffffff
	if ix >= 0x7ff00000 {
		return x + x
	}
	if ix < 0x3e300000 {
		return x
	}
	ax := math.Abs(x)
	var w float64
	switch {
	case ix > 0x41b00000:
		w = logDouble(ax) + fdlibmLn2
	case ix > 0x40000000:
		w = logDouble(2.0*ax + 1/(math.Sqrt(x*x+1)+ax))
	default:
		t := x * x
		w = math.Log1p(ax + t/(1+math.Sqrt(1+t)))
	}
	if hx>>31 == 0 {
		return w
	}
	return -w
}

// fdlibmAcosh is fdlibm's e_acosh.c, as glibc computes acosh.
func fdlibmAcosh(x float64) float64 {
	hx := highWord(x)
	switch {
	case int32(hx) < 0x3ff00000:
		return math.NaN()
	case hx >= 0x41b00000:
		if hx >= 0x7ff00000 {
			return x + x
		}
		return logDouble(x) + fdlibmLn2
	case x == 1:
		return 0
	case hx > 0x40000000:
		t := x * x
		return logDouble(2.0*x - 1/(x+math.Sqrt(t-1)))
	default:
		t := x - 1
		return math.Log1p(t + math.Sqrt(2.0*t+t*t))
	}
}

// fdlibmAtanh is glibc's e_atanh.c.
func fdlibmAtanh(x float64) float64 {
	xa := math.Abs(x)
	var t float64
	switch {
	case xa < 0.5:
		if xa < 0x1p-28 {
			return x
		}
		t = xa + xa
		t = 0.5 * math.Log1p(t+t*xa/(1.0-xa))
	case xa < 1:
		t = 0.5 * math.Log1p((xa+xa)/(1.0-xa))
	case xa > 1:
		return math.NaN()
	default:
		return x / 0.0
	}
	return math.Copysign(t, x)
}

// decimalLargeLog is ln(2x) for x = c * 10^e with e > 34, the asymptote
// bid128_asinh and bid128_acosh use: log(2c) + e*ln(10) in binary128.
func decimalLargeLog(x *apd.Decimal) types.Decimal128 {
	c := new(big.Float).SetPrec(binary128Precision).SetInt(x.Coeff.MathBigInt())
	c.SetMantExp(c, 1)
	scaled := quadRound(new(big.Float).SetPrec(2*binary128Precision).Mul(new(big.Float).SetInt64(int64(x.Exponent)), binary128Ln10))
	sum := new(big.Float).SetPrec(2*binary128Precision).Add(scaled, quadRound(logBig(c, trigWorkPrecision)))
	return binary128ToDecimal(quadRound(sum))
}

// decimalInverseHyperbolic follows bid128_asinh, bid128_acosh and
// bid128_atanh for arguments inside MongoDB's bounds.
func decimalInverseHyperbolic(fn TrigFunc, d types.Decimal128) types.Decimal128 {
	x := SignedDecimalToAPD(d)
	switch {
	case x.Form == apd.NaN:
		return decimalNaN
	case x.Form == apd.Infinite:
		if fn == Asinh || (fn == Acosh && !x.Negative) {
			return APDToDecimal128(x)
		}
		return decimalNaN
	case x.IsZero() && fn != Acosh:
		return withDecimalSign(types.Decimal128{}, x.Negative)
	}

	switch fn {
	case Asinh:
		if x.Exponent > 34 {
			return withDecimalSign(decimalLargeLog(x), x.Negative)
		}
		return binary128ToDecimal(roundedTrig(Asinh, apdToBinary128(x), binary128Precision))

	case Acosh:
		if x.Cmp(decimalAcoshNearOne) < 0 {
			if x.Cmp(apd.New(1, 0)) < 0 {
				return decimalNaN
			}
			z := new(apd.Decimal)
			_, _ = decimal128Context.Sqrt(z, decimalFMAValue(x, x, apd.New(-1, 0)))
			return binary128ToDecimal(roundedTrig(Asinh, apdToBinary128(z), binary128Precision))
		}
		if x.Exponent > 34 {
			return decimalLargeLog(x)
		}
		return binary128ToDecimal(roundedTrig(Acosh, apdToBinary128(x), binary128Precision))

	default:
		if x.Exponent <= -51 {
			return d
		}
		ax := new(apd.Decimal).Abs(x)
		oneMinus := new(apd.Decimal)
		_, _ = decimal128Context.Sub(oneMinus, apd.New(1, 0), ax)
		switch {
		case oneMinus.IsZero():
			return APDToDecimal128(&apd.Decimal{Form: apd.Infinite, Negative: x.Negative})
		case oneMinus.Negative:
			return decimalNaN
		}
		ratio := decimalQuo(ax, oneMinus)
		doubled := new(apd.Decimal)
		_, _ = decimal128Context.Add(doubled, ratio, ratio)
		r := quadRound(log1pBig(apdToBinary128(doubled), trigWorkPrecision))
		r.SetMantExp(r, -1)
		return withDecimalSign(binary128ToDecimal(r), x.Negative)
	}
}
