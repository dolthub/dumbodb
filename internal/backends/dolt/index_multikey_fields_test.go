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

package dolt

import (
	"testing"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestIndexMultikeyFields: an index records which of its fields held arrays
// and keeps them across encoding; one made multikey before they were recorded
// treats every field as multikey.
func TestIndexMultikeyFields(t *testing.T) {
	idx := backends.IndexInfo{Name: "b_1_m_1", Key: []backends.IndexKeyPair{{Field: "b"}, {Field: "m"}}}
	if idx.KeyMultikey(0) || idx.KeyMultikey(1) {
		t.Fatal("a new index is multikey")
	}
	idx.MarkMultikey(1 << 1)
	if idx.KeyMultikey(0) || !idx.KeyMultikey(1) {
		t.Fatalf("after an array in m: b %v, m %v", idx.KeyMultikey(0), idx.KeyMultikey(1))
	}

	decode := func(raw []byte) backends.IndexInfo {
		t.Helper()
		info, err := decodeIndexDefinition(must.NotFail(bson.ToDocument(wirebson.RawDocument(raw))))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	got := decode(must.NotFail(encodeIndexDefinition(idx)))
	if got.KeyMultikey(0) || !got.KeyMultikey(1) {
		t.Fatalf("decoded: b %v, m %v", got.KeyMultikey(0), got.KeyMultikey(1))
	}

	old := idx
	old.MultikeyFieldsKnown = false
	got = decode(must.NotFail(encodeIndexDefinition(old)))
	if !got.KeyMultikey(0) || !got.KeyMultikey(1) {
		t.Fatal("an index without recorded fields is not multikey on every field")
	}
	got.MarkMultikey(1 << 1)
	if !got.KeyMultikey(0) {
		t.Fatal("a later array made unrecorded fields known")
	}

	plain := backends.IndexInfo{Name: "a_1", Key: []backends.IndexKeyPair{{Field: "a"}}}
	if raw := must.NotFail(encodeIndexDefinition(plain)); decode(raw).KeyMultikey(0) {
		t.Fatal("a plain index decodes as multikey")
	}
}
