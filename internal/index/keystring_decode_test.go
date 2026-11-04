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
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestDecodeValueRoundTrips(t *testing.T) {
	for _, v := range []any{
		types.Null, types.Undefined, types.MinKey, types.MaxKey, true, false,
		int32(0), int32(1), int32(-1), int32(255), int32(256), int32(math.MaxInt32), int32(math.MinInt32),
		int64(0), int64(1) << 40, int64(math.MaxInt64), int64(math.MinInt64), int64(-1) << 50,
		float64(0), math.Copysign(0, -1), 2.5, -2.5, 0.1, 1e-300, 3.0, -7.0,
		math.Inf(1), math.Inf(-1), float64(1 << 62), float64(1<<63) * 1.5,
		-0.1, -1e-20, 1e300, -1e300, math.MaxFloat64, math.SmallestNonzeroFloat64, -0x1.8p-256, 0x1p-255, 4503599627370495.5,
		"", "abc", "a\x00b\x00", "\x00",
		types.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		time.UnixMilli(1700000000123).UTC(), time.UnixMilli(-5).UTC(),
		types.Timestamp(42<<32 | 7),
		types.Binary{Subtype: types.BinaryGeneric, B: []byte{0, 1, 2}},
		types.Regex{Pattern: "^a.b", Options: "i"},
	} {
		tag := TypeTags([]any{v})[0]
		got, ok := DecodeValue(EncodeValue(v), tag)
		if !ok {
			t.Errorf("%T %v: not decoded", v, v)
			continue
		}
		if f, isFloat := v.(float64); isFloat {
			g, _ := got.(float64)
			if math.Float64bits(f) != math.Float64bits(g) {
				t.Errorf("%v: decoded %v", f, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, v) {
			t.Errorf("%T %v: decoded %T %v", v, v, got, got)
		}
	}
	if got, ok := DecodeValue(EncodeValue(math.NaN()), TagDouble); !ok || !math.IsNaN(got.(float64)) {
		t.Errorf("NaN: decoded %v, %v", got, ok)
	}
}

func TestDecodeValueRefusesWhatItCannotRestore(t *testing.T) {
	for _, v := range []any{
		must.NotFail(types.NewDocument("a", int32(1))),
		must.NotFail(types.NewArray(int32(1))),
		PreEncoded("sortkey"),
		types.Decimal128{H: 0x3040000000000000, L: 3},
	} {
		if tag := TypeTags([]any{v})[0]; tag != TagFetch {
			t.Errorf("%T: tag %d, want TagFetch", v, tag)
		}
		if _, ok := DecodeValue(EncodeValue(v), TagFetch); ok {
			t.Errorf("%T: decoded", v)
		}
	}
	if _, ok := DecodeValue(EncodeValue(2.5), TagInt32); ok {
		t.Error("a fraction decoded as int32")
	}
}
