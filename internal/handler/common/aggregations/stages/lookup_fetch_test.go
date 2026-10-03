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

package stages

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func doc(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
func arr(values ...any) *types.Array   { return must.NotFail(types.NewArray(values...)) }

func exprEqMatch(a, b any) *types.Document {
	return doc("$match", doc("$expr", doc("$eq", arr(a, b))))
}

func TestLeadingMatchEqualityField(t *testing.T) {
	for name, tc := range map[string]struct {
		pipeline *types.Array
		want     string
	}{
		"field first":        {arr(exprEqMatch("$k", "$$v")), "k"},
		"field second":       {arr(exprEqMatch("$$v", "$k")), "k"},
		"two field paths":    {arr(exprEqMatch("$k", "$j")), ""},
		"not $eq":            {arr(doc("$match", doc("$expr", doc("$gt", arr("$k", "$$v"))))), ""},
		"plain match":        {arr(doc("$match", doc("k", int32(1)))), ""},
		"match not first":    {arr(doc("$limit", int32(1)), exprEqMatch("$k", "$$v")), ""},
		"extra match clause": {arr(doc("$match", doc("$expr", doc("$eq", arr("$k", "$$v")), "x", int32(1)))), ""},
		"empty":              {arr(), ""},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, leadingMatchEqualityField(tc.pipeline))
		})
	}
}

func TestLeadingMatchEqualityValue(t *testing.T) {
	value, ok := leadingMatchEqualityValue(exprEqMatch("$k", int32(5)), "k")
	require.True(t, ok)
	require.Equal(t, int32(5), value)

	_, ok = leadingMatchEqualityValue(exprEqMatch("$k", types.Null), "k")
	require.False(t, ok, "null is not pushed down")

	_, ok = leadingMatchEqualityValue(exprEqMatch("$k", arr(int32(1))), "k")
	require.False(t, ok, "arrays are not pushed down")

	_, ok = leadingMatchEqualityValue(exprEqMatch("$k", int32(5)), "other")
	require.False(t, ok, "a different field is not pushed down")
}

type countingFetcher struct {
	docs    []*types.Document
	fetches int
	indexed bool
}

func (f *countingFetcher) Fetch(_ context.Context, _ string, _ *types.Document) ([]*types.Document, error) {
	f.fetches++
	return f.docs, nil
}

func (f *countingFetcher) IndexedField(context.Context, string, string) bool { return f.indexed }

func TestCachingFetcher_LoadsUnfilteredCollectionOnce(t *testing.T) {
	inner := &countingFetcher{docs: []*types.Document{doc("_id", int32(1))}}
	f := withFetchCache(inner)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := f.Fetch(ctx, "c", nil)
		require.NoError(t, err)
	}
	require.Equal(t, 1, inner.fetches)

	_, err := f.Fetch(ctx, "c", doc("k", int32(1)))
	require.NoError(t, err)
	require.Equal(t, 2, inner.fetches, "filtered fetches are not cached")
	require.Same(t, f, withFetchCache(f), "an existing cache is reused")
}

func TestLookupPipeline_DoesNotModifySharedForeignDocuments(t *testing.T) {
	foreign := []*types.Document{doc("_id", int32(1), "k", "a"), doc("_id", int32(2), "k", "b")}
	fetcher := &countingFetcher{docs: foreign}

	s, err := NewLookupStage(doc("$lookup", doc(
		"from", "f",
		"let", doc("v", "$ref"),
		"pipeline", arr(exprEqMatch("$k", "$$v"), doc("$set", doc("touched", true))),
		"as", "j",
	)), fetcher)
	require.NoError(t, err)

	closer := iterator.NewMultiCloser()
	defer closer.Close()
	input := iterator.Values(iterator.ForSlice([]*types.Document{doc("_id", int32(10), "ref", "a"), doc("_id", int32(11), "ref", "b")}))
	out, err := s.Process(context.Background(), input, closer)
	require.NoError(t, err)
	results, err := iterator.ConsumeValues(out)
	require.NoError(t, err)

	require.Len(t, results, 2)
	for _, r := range results {
		require.Equal(t, 1, must.NotFail(r.Get("j")).(*types.Array).Len())
	}
	for _, f := range foreign {
		require.False(t, f.Has("touched"), "a pipeline stage modified a fetched document")
	}
	require.Equal(t, 1, fetcher.fetches, "an unindexed pipeline lookup loads the collection once")
}

// The hash index must find exactly what comparing against every foreign
// document finds.
func TestEqualityIndex_MatchesFullScan(t *testing.T) {
	const p53 = int64(1) << 53
	values := []any{
		int32(5), int64(5), float64(5), float64(5.5), "5", true, false,
		int32(0), math.Copysign(0, -1), p53, p53 + 1, float64(p53), float64(p53) + 2,
		math.NaN(), math.Inf(1), types.Null, nil,
		arr(int32(5), "x"), arr(), arr(arr(int32(5))),
		doc("a", int32(1)), types.ObjectID{1},
	}

	var foreign []*types.Document
	for i, v := range values {
		if v == nil {
			foreign = append(foreign, doc("_id", int32(i)))
			continue
		}
		foreign = append(foreign, doc("_id", int32(i), "k", v))
	}

	l := &lookup{localField: "ref", foreignField: "k"}
	idx := newEqualityIndex(foreign, "k")

	for _, local := range values {
		input := doc("_id", int32(-1))
		if local != nil {
			input = doc("_id", int32(-1), "ref", local)
		}

		var want []*types.Document
		for _, f := range foreign {
			if lookupValuesMatch(getFieldValue(input, "ref"), getFieldValue(f, "k")) {
				want = append(want, f)
			}
		}

		got := l.equalityMatches(input, idx)
		require.Equal(t, len(want), len(got), "local value %#v", local)
		for i := range want {
			require.Same(t, want[i], got[i], "local value %#v", local)
		}
	}
}

// filteringFetcher reports an index on every field and applies the filter, as
// the backend does.
type filteringFetcher struct {
	docs    []*types.Document
	fetches int
}

func (f *filteringFetcher) Fetch(_ context.Context, _ string, filter *types.Document) ([]*types.Document, error) {
	f.fetches++
	if filter == nil {
		return f.docs, nil
	}
	var out []*types.Document
	for _, d := range f.docs {
		matched, err := common.FilterDocument(d, filter)
		if err != nil {
			return nil, err
		}
		if matched {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *filteringFetcher) IndexedField(context.Context, string, string) bool { return true }

// Fetching each traversal level through an index returns exactly what the
// in-memory traversal returns, in the same order.
func TestGraphLookup_IndexedSourceMatchesInMemory(t *testing.T) {
	emps := []*types.Document{
		doc("_id", int32(5), "name", "e", "boss", "c"),
		doc("_id", int32(1), "name", "a", "boss", types.Null),
		doc("_id", int32(4), "name", "d", "boss", "b"),
		doc("_id", int32(2), "name", "b", "boss", "a"),
		doc("_id", int32(3), "name", "c", "boss", "a"),
		doc("_id", int32(6), "name", "loop1", "boss", "loop2"),
		doc("_id", int32(7), "name", "loop2", "boss", "loop1"),
	}
	inputs := []*types.Document{
		doc("_id", "q1", "start", "e"),
		doc("_id", "q2", "start", "d"),
		doc("_id", "q3", "start", "loop1"),
		doc("_id", "q4", "start", types.Null),
	}
	spec := doc("$graphLookup", doc(
		"from", "emps", "startWith", "$start", "connectFromField", "boss",
		"connectToField", "name", "as", "chain", "depthField", "depth",
	))

	run := func(fetcher CollectionFetcher) []*types.Document {
		s, err := NewGraphLookupStage(spec, fetcher)
		require.NoError(t, err)
		closer := iterator.NewMultiCloser()
		defer closer.Close()
		out, err := s.Process(context.Background(), iterator.Values(iterator.ForSlice(inputs)), closer)
		require.NoError(t, err)
		results, err := iterator.ConsumeValues(out)
		require.NoError(t, err)
		return results
	}

	inMemory := run(FetcherFunc(func(context.Context, string, *types.Document) ([]*types.Document, error) {
		return emps, nil
	}))
	indexed := &filteringFetcher{docs: emps}
	viaIndex := run(indexed)

	require.Len(t, viaIndex, len(inMemory))
	for i := range inMemory {
		require.Equal(t, types.Compare(inMemory[i], viaIndex[i]), types.Equal,
			"input %d: in memory %v, via index %v", i, inMemory[i], viaIndex[i])
	}
	require.Greater(t, indexed.fetches, 1, "levels were not fetched through the index")
}
