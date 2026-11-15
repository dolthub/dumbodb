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
	"testing"

	"github.com/stretchr/testify/require"
	mongobson "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func symbolJavaScriptRaw(t *testing.T) []byte {
	t.Helper()
	raw, err := mongobson.Marshal(mongobson.D{
		{Key: "s", Value: primitive.Symbol("abc")},
		{Key: "j", Value: primitive.JavaScript("function () {}")},
		{Key: "d", Value: mongobson.D{{Key: "s", Value: primitive.Symbol("")}}},
		{Key: "a", Value: mongobson.A{primitive.JavaScript("x"), "x", primitive.Symbol("x")}},
	})
	require.NoError(t, err)
	return raw
}

func TestDecodeRawSymbolJavaScript(t *testing.T) {
	doc, err := DecodeRawDocument(symbolJavaScriptRaw(t))
	require.NoError(t, err)

	s, _ := doc.Get("s")
	require.Equal(t, types.Symbol("abc"), s)
	j, _ := doc.Get("j")
	require.Equal(t, types.JavaScript("function () {}"), j)
	nested, _ := doc.Get("d")
	ns, _ := nested.(*types.Document).Get("s")
	require.Equal(t, types.Symbol(""), ns)
	arr, _ := doc.Get("a")
	want := []any{types.JavaScript("x"), "x", types.Symbol("x")}
	require.Equal(t, len(want), arr.(*types.Array).Len())
	for i, w := range want {
		require.Equal(t, w, must.NotFail(arr.(*types.Array).Get(i)))
	}
}

func TestSymbolJavaScriptRoundTripEncoders(t *testing.T) {
	raw := symbolJavaScriptRaw(t)
	doc, err := DecodeRawDocument(raw)
	require.NoError(t, err)

	require.True(t, bytes.Equal(raw, doc.AppendBSON(nil)), "AppendBSON")

	viaRaw, err := FromDocumentRaw(doc)
	require.NoError(t, err)
	require.True(t, bytes.Equal(raw, viaRaw), "FromDocumentRaw")

	require.True(t, AnyNeedsRawEncoding(doc))
	require.True(t, AnyNeedsRawEncoding(types.Symbol("x")))
	require.False(t, AnyNeedsRawEncoding(must.NotFail(types.NewDocument("s", "x"))))
}

func TestDecodeRawFieldsSkipsSymbolJavaScript(t *testing.T) {
	doc, err := DecodeRawDocumentFields(symbolJavaScriptRaw(t), []string{"d"})
	require.NoError(t, err)
	require.Equal(t, []string{"d"}, doc.Keys())
}

func TestDecodeRawSymbolRejectsTruncatedString(t *testing.T) {
	raw := symbolJavaScriptRaw(t)
	// The Symbol length prefix starts after the 4-byte document length,
	// the tag byte and the "s\x00" field name.
	raw[7] = 0xff
	_, err := DecodeRawDocument(raw)
	require.Error(t, err)
}
