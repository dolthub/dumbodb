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

package bson

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/FerretDB/wire/wirebson"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func decodeRawTestDocs() map[string]*types.Document {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	nested := doc("a", int32(1), "b", arr("x", types.Null, doc("deep", arr(arr()))))
	twelve := types.MakeArray(12)
	for i := 0; i < 12; i++ {
		twelve.Append(int64(i))
	}

	return map[string]*types.Document{
		"empty": doc(),
		"every type": doc(
			"_id", types.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			"double", 1.5,
			"nan", math.NaN(),
			"neg zero", math.Copysign(0, -1),
			"string", "hello",
			"empty string", "",
			"unicode", "héllo 世界",
			"document", nested,
			"empty document", doc(),
			"array", arr(int32(1), "two", nested),
			"empty array", arr(),
			"binary", types.Binary{B: []byte{1, 2, 3}, Subtype: types.BinaryUser},
			"empty binary", types.Binary{B: []byte{}, Subtype: types.BinaryGeneric},
			"true", true,
			"false", false,
			"date", time.Date(2026, 10, 6, 1, 2, 3, 4000000, time.UTC),
			"pre-epoch date", time.Date(1901, 1, 1, 0, 0, 0, 0, time.UTC),
			"null", types.Null,
			"regex", types.Regex{Pattern: "^a.*", Options: "im"},
			"empty regex", types.Regex{},
			"int32", int32(-7),
			"timestamp", types.Timestamp(42),
			"int64", int64(math.MinInt64),
			"decimal", types.Decimal128{L: 1, H: 2},
		),
		"many array elements":        doc("a", twelve),
		"top-level min and max keys": doc("lo", types.MinKey, "x", int32(1), "hi", types.MaxKey),
		"nested min and max keys":    doc("d", doc("lo", types.MinKey, "hi", types.MaxKey)),
		"large string":               doc("pad", strings.Repeat("x", 600*1024)),
	}
}

func TestDecodeRawDocumentMatchesToDocument(t *testing.T) {
	for name, want := range decodeRawTestDocs() {
		t.Run(name, func(t *testing.T) {
			raw, err := FromDocumentRaw(want)
			require.NoError(t, err)

			viaWirebson, err := decodeViaWirebson(raw)
			require.NoError(t, err)
			direct, err := DecodeRawDocument(raw)
			require.NoError(t, err)

			requireSameValue(t, viaWirebson, direct)
			require.Equal(t, types.Equal, types.Compare(want, direct))
		})
	}
}

func TestDecodeRawDocumentMinMaxKeyInArray(t *testing.T) {
	want := must.NotFail(types.NewDocument("a", must.NotFail(types.NewArray(types.MinKey, int32(1), types.MaxKey))))
	raw, err := FromDocumentRaw(want)
	require.NoError(t, err)
	got, err := DecodeRawDocument(raw)
	require.NoError(t, err)
	require.Equal(t, types.Equal, types.Compare(want, got))
}

func TestDecodeRawDocumentRejectsMalformed(t *testing.T) {
	valid := must.NotFail(FromDocumentRaw(must.NotFail(types.NewDocument("a", "b", "c", int32(1)))))
	for name, raw := range map[string][]byte{
		"empty":              {},
		"short":              {5, 0, 0, 0},
		"length too long":    append([]byte{99}, valid[1:]...),
		"truncated":          valid[:len(valid)-2],
		"missing terminator": append(append([]byte(nil), valid[:len(valid)-1]...), 1),
		"bad bool":           {9, 0, 0, 0, 0x08, 'b', 0, 2, 0},
		"bad array index":    {20, 0, 0, 0, 0x04, 'a', 0, 12, 0, 0, 0, 0x10, '5', 0, 1, 0, 0, 0, 0, 0},
		"unsupported tag":    {8, 0, 0, 0, 0x06, 'u', 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeRawDocument(raw)
			require.Error(t, err)
		})
	}
}

func decodeViaWirebson(raw []byte) (*types.Document, error) {
	if doc, err := ToDocumentHandlingMinMaxKey(wirebson.RawDocument(raw)); doc != nil || err != nil {
		return doc, err
	}
	return ToDocument(wirebson.RawDocument(raw))
}

// Both decoders must agree on every input, except that only DecodeRawDocument
// accepts MinKey or MaxKey inside arrays.
func FuzzDecodeRawDocument(f *testing.F) {
	for _, d := range decodeRawTestDocs() {
		f.Add(must.NotFail(FromDocumentRaw(d)))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		direct, directErr := DecodeRawDocument(raw)
		viaWirebson, wireErr := safeDecodeViaWirebson(raw)

		if directErr == nil && wireErr != nil {
			if bytes.IndexByte(raw, bsonTagMinKey) >= 0 || bytes.IndexByte(raw, bsonTagMaxKey) >= 0 {
				return
			}
			t.Fatalf("direct decoder accepted input the wirebson path rejects (%v): %x", wireErr, raw)
		}
		if directErr != nil && wireErr == nil {
			t.Fatalf("direct decoder rejected input the wirebson path accepts (%v): %x", directErr, raw)
		}
		if directErr == nil {
			requireSameValue(t, viaWirebson, direct)
		}
	})
}

func safeDecodeViaWirebson(raw []byte) (doc *types.Document, err error) {
	defer func() {
		if r := recover(); r != nil {
			doc, err = nil, wirebson.ErrDecodeInvalidInput
		}
	}()
	return decodeViaWirebson(raw)
}

// requireSameValue compares decoded values exactly, with floats by bit pattern
// so that NaN and -0 are checked too.
func requireSameValue(t *testing.T, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case *types.Document:
		g, ok := got.(*types.Document)
		require.True(t, ok, "got %T, want *types.Document", got)
		require.Equal(t, w.Keys(), g.Keys())
		wv, gv := w.Values(), g.Values()
		for i := range wv {
			requireSameValue(t, wv[i], gv[i])
		}
	case *types.Array:
		g, ok := got.(*types.Array)
		require.True(t, ok, "got %T, want *types.Array", got)
		require.Equal(t, w.Len(), g.Len())
		for i := 0; i < w.Len(); i++ {
			requireSameValue(t, must.NotFail(w.Get(i)), must.NotFail(g.Get(i)))
		}
	case float64:
		g, ok := got.(float64)
		require.True(t, ok, "got %T, want float64", got)
		require.Equal(t, math.Float64bits(w), math.Float64bits(g))
	case types.Binary:
		g, ok := got.(types.Binary)
		require.True(t, ok, "got %T, want types.Binary", got)
		require.Equal(t, w.Subtype, g.Subtype)
		require.Equal(t, w.B == nil, g.B == nil)
		require.Equal(t, w.B, g.B)
	default:
		require.Equal(t, want, got)
	}
}

func TestDecodeRawDocumentFieldsKeepsOnlyNamedFields(t *testing.T) {
	for name, doc := range decodeRawTestDocs() {
		t.Run(name, func(t *testing.T) {
			raw := doc.AppendBSON(nil)
			for _, fields := range [][]string{{}, {"_id"}, {"document", "int32", "missing"}, doc.Keys()} {
				got, err := DecodeRawDocumentFields(raw, fields)
				require.NoError(t, err)
				requireSameValue(t, keepFields(t, doc, fields), got)
			}
		})
	}
}

func FuzzDecodeRawDocumentFields(f *testing.F) {
	for _, d := range decodeRawTestDocs() {
		f.Add(d.AppendBSON(nil), "_id")
	}
	f.Fuzz(func(t *testing.T, raw []byte, field string) {
		full, err := DecodeRawDocument(raw)
		if err != nil {
			return
		}
		got, err := DecodeRawDocumentFields(raw, []string{field})
		require.NoError(t, err)
		requireSameValue(t, keepFields(t, full, []string{field}), got)
	})
}

func keepFields(t *testing.T, doc *types.Document, fields []string) *types.Document {
	t.Helper()
	keys, values := doc.Keys(), doc.Values()
	kept := new(types.Document)
	for i, k := range keys {
		for _, f := range fields {
			if k == f {
				kept.AppendDecoded(k, values[i])
				break
			}
		}
	}
	return kept
}
