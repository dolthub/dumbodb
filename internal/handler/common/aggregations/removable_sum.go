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
	"strconv"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/types"
)

// RemovableSum is mongod's $sum and $avg state for a window with a lower
// bound. A value leaving the window is added negated to one running NumberSum,
// so its rounding and overflow stay in the total. NaN and infinite inputs are
// counted rather than summed, and the result narrows to the types still in the
// window.
type RemovableSum struct {
	sum                 *NumberSum
	posInf, negInf, nan int64
	doubles, decimals   int64
	count               int64
}

// NewRemovableSum returns an empty window sum.
func NewRemovableSum() *RemovableSum {
	return &RemovableSum{sum: NewNumberSum()}
}

// Add folds a value entering the window; non-numeric values are ignored.
func (r *RemovableSum) Add(v any) { r.update(v, 1) }

// Remove takes back a value leaving the window.
func (r *RemovableSum) Remove(v any) { r.update(v, -1) }

func (r *RemovableSum) update(v any, quantity int64) {
	switch n := v.(type) {
	case int32:
		if n == math.MinInt32 && quantity < 0 {
			r.sum.Add(int32(math.MaxInt32))
			r.sum.Add(int32(1))
		} else {
			r.sum.Add(n * int32(quantity))
		}
	case int64:
		if n == math.MinInt64 && quantity < 0 {
			r.sum.Add(int64(math.MaxInt64))
			r.sum.Add(int32(1))
		} else {
			r.sum.Add(n * quantity)
		}
	case float64:
		r.doubles += quantity
		switch {
		case math.IsNaN(n):
			r.nan += quantity
		case math.IsInf(n, 1):
			r.posInf += quantity
		case math.IsInf(n, -1):
			r.negInf += quantity
		default:
			r.sum.Add(n * float64(quantity))
		}
	case types.Decimal128:
		r.decimals += quantity
		x := DecimalToAPD(n)
		switch {
		case x.Form == apd.NaN || x.Form == apd.NaNSignaling:
			r.nan += quantity
		case x.Form == apd.Infinite && !x.Negative:
			r.posInf += quantity
		case x.Form == apd.Infinite:
			r.negInf += quantity
		default:
			if quantity < 0 {
				n.H ^= 1 << 63
			}
			r.sum.Add(n)
		}
	default:
		return
	}

	r.count += quantity
}

// Sum returns the window's $sum.
func (r *RemovableSum) Sum() any {
	switch {
	case r.nan > 0 || (r.posInf > 0 && r.negInf > 0):
		return r.special(apd.NaN, false)
	case r.posInf > 0:
		return r.special(apd.Infinite, false)
	case r.negInf > 0:
		return r.special(apd.Infinite, true)
	}

	switch v := r.sum.Result().(type) {
	case types.Decimal128:
		if r.decimals > 0 {
			return v
		}
		f := decimalToFloat64(v)
		if r.doubles > 0 {
			return f
		}
		if n, ok := decimalToNearestInt64(v); ok {
			return intOrLong(n)
		}
		return f
	case float64:
		if r.doubles == 0 && v >= math.MinInt64 && v < math.MaxInt64 {
			return intOrLong(int64(math.Round(v)))
		}
		return v
	case int64:
		return intOrLong(v)
	default:
		return v
	}
}

// Avg returns the window's $avg: null for no numeric values, otherwise Sum
// over the count, with a NaN or infinite sum returned as is.
func (r *RemovableSum) Avg() any {
	if r.count == 0 {
		return types.Null
	}

	switch v := r.Sum().(type) {
	case int32:
		return float64(v) / float64(r.count)
	case int64:
		return float64(v) / float64(r.count)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return v
		}
		return v / float64(r.count)
	case types.Decimal128:
		return avgFromDecimalSum(v, r.count)
	default:
		return v
	}
}

func (r *RemovableSum) special(form apd.Form, negative bool) any {
	if r.decimals > 0 {
		return APDToDecimal128(&apd.Decimal{Form: form, Negative: negative})
	}
	if form == apd.NaN {
		return math.NaN()
	}
	if negative {
		return math.Inf(-1)
	}
	return math.Inf(1)
}

// decimalToFloat64 rounds a finite Decimal128 to the nearest double,
// overflowing to an infinity.
func decimalToFloat64(d types.Decimal128) float64 {
	f, _ := strconv.ParseFloat(DecimalToAPD(d).String(), 64)
	return f
}

// decimalToNearestInt64 rounds d half to even; ok is false when it does not
// fit an int64.
func decimalToNearestInt64(d types.Decimal128) (int64, bool) {
	x := DecimalToAPD(d)
	var rounded apd.Decimal
	if _, err := decimal128Context.RoundToIntegralValue(&rounded, x); err != nil {
		return 0, false
	}
	n, err := rounded.Int64()
	return n, err == nil
}
