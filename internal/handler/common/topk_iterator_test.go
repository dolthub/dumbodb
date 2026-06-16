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

package common

import (
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// topKReferenceIDs returns the "id" sequence of a full stable sort by sortDoc
// truncated to k, the behavior TopKIterator must reproduce.
func topKReferenceIDs(t *testing.T, docs []*types.Document, sortDoc *types.Document, k int64) []int32 {
	t.Helper()

	cp := make([]*types.Document, len(docs))
	copy(cp, docs)

	if err := SortDocuments(cp, sortDoc); err != nil {
		t.Fatalf("SortDocuments: %v", err)
	}

	if int64(len(cp)) > k {
		cp = cp[:k]
	}

	return docIDs(t, cp)
}

func docIDs(t *testing.T, docs []*types.Document) []int32 {
	t.Helper()

	ids := make([]int32, len(docs))
	for i, d := range docs {
		ids[i] = must.NotFail(d.Get("id")).(int32)
	}

	return ids
}

func TestTopKIterator(t *testing.T) {
	sortDoc := must.NotFail(types.NewDocument("v", int32(1)))

	// values include heavy ties so the tie-stability path is exercised.
	values := []int32{5, 3, 3, 1, 9, 3, 1, 7, 2, 8, 3, 1, 6, 4, 0, 3, 1, 5, 2, 9}

	build := func() []*types.Document {
		docs := make([]*types.Document, len(values))
		for i, v := range values {
			docs[i] = must.NotFail(types.NewDocument("v", v, "id", int32(i)))
		}

		return docs
	}

	for _, k := range []int64{1, 3, 5, 10, 20, 50} {
		docs := build()

		closer := iterator.NewMultiCloser()
		iter, err := TopKIterator(iterator.Values(iterator.ForSlice(docs)), closer, sortDoc, k)
		if err != nil {
			t.Fatalf("k=%d: TopKIterator: %v", k, err)
		}

		got, err := iterator.ConsumeValues(iter)
		closer.Close()
		if err != nil {
			t.Fatalf("k=%d: consume: %v", k, err)
		}

		want := topKReferenceIDs(t, build(), sortDoc, k)
		gotIDs := docIDs(t, got)

		if len(gotIDs) != len(want) {
			t.Fatalf("k=%d: len got=%d want=%d", k, len(gotIDs), len(want))
		}

		for i := range want {
			if gotIDs[i] != want[i] {
				t.Fatalf("k=%d: position %d: got id=%d want id=%d\n got=%v\nwant=%v",
					k, i, gotIDs[i], want[i], gotIDs, want)
			}
		}
	}
}
