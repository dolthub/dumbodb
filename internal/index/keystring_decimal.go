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

package index

import (
	"encoding/binary"
	"math"
	"math/big"

	"github.com/dolthub/dumbodb/internal/types"
)

// A Decimal128 that is exactly a double, or an integer below 2^63, encodes as
// that number. Any other decimal encodes as the double nearest it toward
// zero, with two flag bits in the low bits of the double's encoding and, unless
// the decimal is that double rounded up to 15 significant digits, an 8-byte
// continuation: the decimal's 34-digit coefficient, at the scale of the
// double's decade, less the double's. Large magnitudes have one flag bit and
// always carry the continuation. An integer part of 2^53 or more, where a
// double has no fraction bits, is the decimal's own, flagged above. Decimals
// beyond the double range encode as [range bit][14-bit biased exponent]
// [113-bit coefficient] in 16 bytes.
//
// Below 2^-255 an inexact decimal is the 2^768-scaled double nearest it toward
// zero, a set flag bit, and a continuation. mongod derives the continuation
// from Decimal128 arithmetic on the scaled value; this encoding uses the exact
// value instead. The leading 9 bytes, order and uniqueness match mongod; the
// continuation may not (docs/reference/mongodb-keystring.md, "Known
// deviation").
const (
	decimalEqual15 = 2 // no continuation: the double rounded up to 15 digits
	decimalBelow   = 1 // continuation; below the double rounded up to 15 digits
	decimalAbove   = 3 // continuation; above it
)

var (
	smallestDoubleBits = smallBits(math.SmallestNonzeroFloat64)
	largestDoubleBits  = largeBits(math.MaxFloat64)
)

type decimal struct {
	negative bool
	coef     *big.Int // 0 <= coef < 10^34
	exp      int      // value = coef * 10^exp
	inf, nan bool
}

func parseDecimal(d types.Decimal128) decimal {
	out := decimal{negative: d.H>>63 == 1}
	switch {
	case d.H>>58&0x1F == 0x1F:
		out.nan = true
		return out
	case d.H>>58&0x1F == 0x1E:
		out.inf = true
		return out
	case d.H>>61&3 == 3:
		// The 11-prefixed coefficient form exceeds 10^34-1: non-canonical, zero.
		out.coef = new(big.Int)
		out.exp = int(d.H>>47&0x3FFF) - 6176
		return out
	}
	out.exp = int(d.H>>49&0x3FFF) - 6176
	out.coef = new(big.Int).Lsh(new(big.Int).SetUint64(d.H&(1<<49-1)), 64)
	out.coef.Or(out.coef, new(big.Int).SetUint64(d.L))
	if out.coef.Cmp(maxCoefficient) > 0 {
		out.coef.SetInt64(0)
	}
	return out
}

var (
	maxCoefficient = new(big.Int).Sub(pow10(34), big.NewInt(1))
	two53          = new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 53))
	two63          = new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 63))
	ratOne         = big.NewRat(1, 1)
	maxDouble      = new(big.Rat).SetFloat64(math.MaxFloat64)
	smallestDouble = new(big.Rat).SetFloat64(math.SmallestNonzeroFloat64)
	twoMinus255    = new(big.Rat).SetFloat64(0x1p-255)
	two768         = new(big.Rat).SetFloat64(0x1p768)
)

// pow10 returns 10^n. The result is shared and must not be modified.
func pow10(n int) *big.Int {
	if n < len(pow10Table) {
		return pow10Table[n]
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// pow10Table holds the powers of ten that values in the double range use.
var pow10Table = func() []*big.Int {
	t := make([]*big.Int, 700)
	t[0] = big.NewInt(1)
	for i := 1; i < len(t); i++ {
		t[i] = new(big.Int).Mul(t[i-1], big.NewInt(10))
	}
	return t
}()

// rat returns |value| exactly.
func (d decimal) rat() *big.Rat {
	r := new(big.Rat).SetInt(d.coef)
	if d.exp >= 0 {
		return r.Mul(r, new(big.Rat).SetInt(pow10(d.exp)))
	}
	return r.Quo(r, new(big.Rat).SetInt(pow10(-d.exp)))
}

func appendDecimal128(out []byte, d128 types.Decimal128) []byte {
	d := parseDecimal(d128)
	switch {
	case d.nan:
		return append(out, ctypeNumeric)
	case d.inf && d.negative:
		return appendFloat64(out, math.Inf(-1))
	case d.inf:
		return appendFloat64(out, math.Inf(1))
	case d.coef.Sign() == 0:
		return append(out, ctypeZero)
	}
	mag := d.rat()
	if f, exact := mag.Float64(); exact {
		if d.negative {
			f = -f
		}
		return appendFloat64(out, f)
	}
	if mag.IsInt() && mag.Cmp(two63) < 0 {
		return appendMagnitude(out, d.negative, mag.Num().Uint64(), 0, 0)
	}

	switch {
	case mag.Cmp(ratOne) < 0:
		if mag.Cmp(smallestDouble) < 0 {
			return appendWide(out, d, ctypePosSmall, ctypeNegSmall, false)
		}
		start := len(out)
		if mag.Cmp(twoMinus255) < 0 {
			// Below 2^-255 the encoding scales by 2^768 first, so the leading
			// double keeps full precision even where a double is subnormal. A
			// double's word ends in a 0 bit; an inexact decimal sets it and
			// always carries the continuation, so it sorts strictly between
			// its truncated double and the next one and never shares a
			// double's bytes.
			scaled := truncQuo(new(big.Int).Lsh(mag.Num(), 768), mag.Denom())
			lead := new(big.Rat).Quo(new(big.Rat).SetFloat64(scaled), two768)
			out = binary.BigEndian.AppendUint64(append(out, ctypePosSmall), math.Float64bits(scaled)<<1|1)
			out = appendContinuation(out, mag, lead)
			return signed(out, start, d.negative, ctypeNegSmall)
		}
		f := truncDouble(mag)
		lead := new(big.Rat).SetFloat64(f)
		flag := decimalFlag(mag, lead)
		out = binary.BigEndian.AppendUint64(append(out, ctypePosSmall), smallBits(f)|uint64(flag))
		if flag != decimalEqual15 {
			out = appendContinuation(out, mag, lead)
		}
		return signed(out, start, d.negative, ctypeNegSmall)
	case mag.Cmp(two63) < 0:
		ip := new(big.Int).Quo(mag.Num(), mag.Denom())
		lead := truncDouble(mag)
		var frac uint64
		fracBits := 0
		leadValue := new(big.Rat).SetInt(ip)
		if mag.Cmp(two53) < 0 {
			fracBits = fractionBits(lead)
			frac = uint64((lead - math.Trunc(lead)) * math.Ldexp(1, fracBits))
			leadValue.SetFloat64(lead)
		}
		start := len(out)
		out = appendMagnitude(out, false, ip.Uint64(), frac, max(fracBits, 1))
		if n := int(out[start]-ctypePosInt1) + 1; n < 8 {
			flag := decimalAbove
			if mag.Cmp(two53) < 0 {
				flag = decimalFlag(mag, leadValue)
			}
			out[len(out)-1] = out[len(out)-1]&^3 | byte(flag)
			if flag == decimalEqual15 {
				return signedMagnitude(out, start, d.negative)
			}
		}
		out = appendContinuation(out, mag, leadValue)
		return signedMagnitude(out, start, d.negative)
	}
	if mag.Cmp(maxDouble) > 0 {
		return appendWide(out, d, ctypePosLarge, ctypeNegLarge, true)
	}
	lead := truncDouble(mag)
	start := len(out)
	out = binary.BigEndian.AppendUint64(append(out, ctypePosLarge), largeBits(lead)|1)
	out = appendContinuation(out, mag, new(big.Rat).SetFloat64(lead))
	return signed(out, start, d.negative, ctypeNegLarge)
}

// truncDouble is the largest double not above mag <= MaxFloat64.
func truncDouble(mag *big.Rat) float64 {
	return truncQuo(mag.Num(), mag.Denom())
}

// truncQuo is the largest double not above num/den, in the normal range.
func truncQuo(num, den *big.Int) float64 {
	n := new(big.Float).SetInt(num)
	d := new(big.Float).SetInt(den)
	f, _ := new(big.Float).SetPrec(53).SetMode(big.ToZero).Quo(n, d).Float64()
	return f
}

// decimalFlag compares mag with lead rounded up to 15 significant digits.
func decimalFlag(mag, lead *big.Rat) int {
	e := decade(lead) - 14
	n, exact := scaledFloor(lead, e)
	if !exact {
		n.Add(n, big.NewInt(1))
	}
	switch cmpScaled(mag, n, e) {
	case 0:
		return decimalEqual15
	case -1:
		return decimalBelow
	}
	return decimalAbove
}

// The helpers below work on a Rat's numerator and denominator directly:
// big.Rat reduces every result by a GCD, which over the 1000-bit
// denominators of tiny values costs far more than the arithmetic.

// decade returns floor(log10(r)) for r > 0. The bit lengths of numerator and
// denominator put it within one of the answer.
func decade(r *big.Rat) int {
	bits := r.Num().BitLen() - r.Denom().BitLen()
	e := int(math.Floor(float64(bits-1) * math.Log10(2)))
	for cmpScaled(r, big.NewInt(1), e+1) >= 0 {
		e++
	}
	for cmpScaled(r, big.NewInt(1), e) < 0 {
		e--
	}
	return e
}

// cmpScaled compares r with n * 10^e.
func cmpScaled(r *big.Rat, n *big.Int, e int) int {
	left := new(big.Int).Set(r.Num())
	right := new(big.Int).Mul(n, r.Denom())
	if e >= 0 {
		right.Mul(right, pow10(e))
	} else {
		left.Mul(left, pow10(-e))
	}
	return left.Cmp(right)
}

// scaledFloor returns floor(r / 10^e) and whether the division is exact.
func scaledFloor(r *big.Rat, e int) (*big.Int, bool) {
	num := new(big.Int).Set(r.Num())
	den := new(big.Int).Set(r.Denom())
	if e >= 0 {
		den.Mul(den, pow10(e))
	} else {
		num.Mul(num, pow10(-e))
	}
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	return q, rem.Sign() == 0
}

// appendContinuation appends mag, a decimal's magnitude, at the scale of lead's
// decade (34 digits there) less lead's, rounded up.
func appendContinuation(out []byte, mag, lead *big.Rat) []byte {
	u := decade(lead) - 33
	c, _ := scaledFloor(mag, u)
	l, exact := scaledFloor(lead, u)
	if !exact {
		l.Add(l, big.NewInt(1))
	}
	return binary.BigEndian.AppendUint64(out, c.Sub(c, l).Uint64())
}

// appendWide appends a decimal beyond the double range: the range bit, the
// biased exponent and the coefficient scaled up to 34 digits as far as the
// exponent allows.
func appendWide(out []byte, d decimal, pos, neg byte, large bool) []byte {
	coef := new(big.Int).Set(d.coef)
	exp := d.exp
	for len(coef.String()) < 34 && exp > -6176 {
		coef.Mul(coef, big.NewInt(10))
		exp--
	}
	word := new(big.Int).Lsh(big.NewInt(int64(exp+6176)), 113)
	word.Or(word, coef)
	if large {
		word.SetBit(word, 127, 1)
	}
	var b [16]byte
	word.FillBytes(b[:])
	start := len(out)
	out = append(append(out, pos), b[:]...)
	return signed(out, start, d.negative, neg)
}

func signed(out []byte, start int, negative bool, neg byte) []byte {
	if negative {
		out[start] = neg
		complement(out[start+1:])
	}
	return out
}

// signedMagnitude negates an integer-part encoding written as positive.
func signedMagnitude(out []byte, start int, negative bool) []byte {
	if negative {
		n := int(out[start]-ctypePosInt1) + 1
		out[start] = ctypeNegInt1 - byte(n-1)
		complement(out[start+1:])
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
