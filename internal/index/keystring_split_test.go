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
	"bytes"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestSplitKeyValues(t *testing.T) {
	oid := types.ObjectID{1, 2, 3}
	for _, vals := range [][]any{
		{int32(2), int32(7)},
		{int32(-300), "k"},
		{2.5, int32(1)},
		{-2.5, 3.25},
		{"a\x00b", int64(1) << 40},
		{nil, true, time.UnixMilli(5)},
		{oid, types.Regex{Pattern: "^a", Options: "i"}, types.Timestamp(9)},
		{must.NotFail(types.NewDocument("x", int32(1))), types.Binary{Subtype: 0, B: []byte{1, 2}}},
		{int32(0), types.MaxKey, types.MinKey},
	} {
		var key []byte
		for _, v := range vals {
			key = append(key, EncodeValue(v)...)
		}
		got, ok := SplitKeyValues(key, len(vals))
		if !ok {
			t.Errorf("%v: did not split", vals)
			continue
		}
		for i, v := range vals {
			if !bytes.Equal(got[i], EncodeValue(v)) {
				t.Errorf("%v: value %d = %x, want %x", vals, i, got[i], EncodeValue(v))
			}
		}
	}

	bin := append(EncodeValue(types.Binary{B: []byte{7}}), EncodeValue(int32(1))...)
	if _, ok := SplitKeyValues(bin, 2); !ok {
		t.Error("a BinData value before the last splits by its length")
	}
	// int32(5) with a following string whose bytes also read as a fraction
	// plus a valid tail would be ambiguous; an 8-byte string is not.
	if _, ok := SplitKeyValues(append(EncodeValue(int32(5)), EncodeValue("abcdefg")...), 2); !ok {
		t.Error("an integer followed by a string splits one way")
	}
}
