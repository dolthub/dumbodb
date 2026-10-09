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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestAppendBSONMatchesExistingEncoders(t *testing.T) {
	docs := decodeRawTestDocs()
	docs["min and max keys in array"] = must.NotFail(types.NewDocument("a", must.NotFail(types.NewArray(types.MinKey, int32(1), types.MaxKey))))
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			want, err := FromDocumentRaw(doc)
			require.NoError(t, err)

			got := doc.AppendBSON(nil)
			require.Equal(t, want, got)
			require.Len(t, got, doc.BSONSize())

			if !AnyContainsMinMaxKey(doc) {
				viaWirebson, err := must.NotFail(FromDocument(doc)).Encode()
				require.NoError(t, err)
				require.Equal(t, []byte(viaWirebson), got)
			}

			prefixed := doc.AppendBSON([]byte{0xAB})
			require.Equal(t, byte(0xAB), prefixed[0])
			require.Equal(t, want, prefixed[1:])
		})
	}
}

func FuzzAppendBSON(f *testing.F) {
	for _, d := range decodeRawTestDocs() {
		f.Add(must.NotFail(FromDocumentRaw(d)))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		doc, err := DecodeRawDocument(raw)
		if err != nil {
			return
		}
		want, err := FromDocumentRaw(doc)
		require.NoError(t, err)
		require.Equal(t, want, doc.AppendBSON(nil))
		require.Equal(t, raw, doc.AppendBSON(nil))
	})
}
