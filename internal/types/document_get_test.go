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

package types

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentGet_MissingKeyMessage(t *testing.T) {
	doc, err := NewDocument("a", int32(1))
	require.NoError(t, err)
	_, err = doc.Get("b")
	require.EqualError(t, err, `types.Document.Get: key not found: "b"`)

	var nilDoc *Document
	_, err = nilDoc.Get("b")
	require.EqualError(t, err, `types.Document.Get: key not found: "b" (nil document)`)
}

// TestDocumentKeyIndex checks that Get, Set and Has on a wide document, which
// look keys up in its index, agree with its fields after every change.
func TestDocumentKeyIndex(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	doc := MakeDocument(0)
	for step := range 5000 {
		key := fmt.Sprintf("k%d", rng.IntN(60))
		switch op := rng.IntN(10); {
		case op < 6:
			doc.Set(key, int32(step))
		case op < 8:
			doc.Remove(key)
		case op == 8:
			doc.Set("_id", int32(step))
			doc.moveIDToTheFirstIndex()
		default:
			doc.SortFieldsByKey()
		}
		for i := range doc.Len() {
			k, v := doc.FieldAt(i)
			got, err := doc.Get(k)
			require.NoError(t, err)
			require.Equal(t, v, got, "step %d key %s", step, k)
		}
		for i := range 60 {
			k := fmt.Sprintf("k%d", i)
			require.Equal(t, slices.Contains(doc.Keys(), k), doc.Has(k))
		}
	}
}
