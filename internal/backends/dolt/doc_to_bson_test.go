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
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// sortDocumentKeys is the reference for docToBSON's key order: it deep-copies doc with object keys lex-sorted at every
// level. Top-level _id is preserved verbatim: MongoDB treats {a:1,b:2}
// and {b:2,a:1} as distinct _ids, and hashID hashes the original byte
// order; sorting would break post-lookup filter equality.
func sortDocumentKeys(doc *types.Document) *types.Document {
	keys := append([]string(nil), doc.Keys()...)
	sort.Strings(keys)
	out := types.MakeDocument(len(keys))
	for _, k := range keys {
		v, _ := doc.Get(k)
		if k == "_id" {
			out.Set(k, v)
			continue
		}
		out.Set(k, sortAnyKeys(v))
	}
	return out
}

func sortAnyKeys(v any) any {
	switch t := v.(type) {
	case *types.Document:
		return sortDocumentKeys(t)
	case *types.Array:
		out := types.MakeArray(t.Len())
		for i := 0; i < t.Len(); i++ {
			el, _ := t.Get(i)
			out.Append(sortAnyKeys(el))
		}
		return out
	default:
		return v
	}
}

func randomSortTestValue(r *rand.Rand, depth int) any {
	switch n := r.Intn(6); {
	case depth < 3 && n == 0:
		return randomSortTestDoc(r, depth+1)
	case depth < 3 && n == 1:
		arr := types.MakeArray(3)
		for i := 0; i < r.Intn(4); i++ {
			arr.Append(randomSortTestValue(r, depth+1))
		}
		return arr
	case n == 2:
		return "s"
	default:
		return int32(n)
	}
}

func randomSortTestDoc(r *rand.Rand, depth int) *types.Document {
	keys := []string{"_id", "b", "A", "a", "z", "Z", "_x", "aa", "10", "9"}
	r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	doc := types.MakeDocument(0)
	for _, k := range keys[:r.Intn(len(keys))] {
		if k == "_id" && depth == 0 {
			doc.Set(k, randomSortTestDoc(r, depth+1))
			continue
		}
		doc.Set(k, randomSortTestValue(r, depth))
	}
	return doc
}

func TestDocToBSONSortsKeysLikeSortDocumentKeys(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		doc := randomSortTestDoc(r, 0)
		want := append([]byte{bsonFormatVersion}, sortDocumentKeys(doc).AppendBSON(nil)...)
		got := must.NotFail(docToBSON(doc))
		require.Equal(t, want, got, "document %v", doc)
	}
}
