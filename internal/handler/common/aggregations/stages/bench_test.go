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

package stages_test

// Unit-level benchmarks for aggregation stages. These exercise the stage
// Process path directly against an in-memory slice iterator, isolating the
// aggregation CPU cost from the chunk-store scan path.

import (
	"context"
	"testing"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

const benchN = 1000

func makeBenchDocs() []*types.Document {
	docs := make([]*types.Document, benchN)
	for i := 0; i < benchN; i++ {
		docs[i] = must.NotFail(types.NewDocument(
			"_id", int32(i),
			"i", int32(i),
			"grp", int32(i%10),
			"tag", "tag",
			"payload", "small",
		))
	}
	return docs
}

func drain(iter types.DocumentsIterator) int {
	n := 0
	for {
		_, _, err := iter.Next()
		if err != nil {
			return n
		}
		n++
	}
}

func BenchmarkStage_MatchGroup(b *testing.B) {
	docs := makeBenchDocs()

	matchSpec := must.NotFail(types.NewDocument("grp",
		must.NotFail(types.NewDocument("$gte", int32(0)))))
	matchStage, err := stages.NewStage(must.NotFail(types.NewDocument("$match", matchSpec)))
	if err != nil {
		b.Fatal(err)
	}
	groupSpec := must.NotFail(types.NewDocument(
		"_id", "$grp",
		"n", must.NotFail(types.NewDocument("$sum", int32(1))),
	))
	groupStage, err := stages.NewStage(must.NotFail(types.NewDocument("$group", groupSpec)))
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		closer := iterator.NewMultiCloser()
		in := iterator.Values(iterator.ForSlice(docs))
		mid, err := matchStage.Process(context.Background(), in, closer)
		if err != nil {
			b.Fatal(err)
		}
		out, err := groupStage.Process(context.Background(), mid, closer)
		if err != nil {
			b.Fatal(err)
		}
		drain(out)
		closer.Close()
	}
}

func BenchmarkStage_Project(b *testing.B) {
	docs := makeBenchDocs()

	projSpec := must.NotFail(types.NewDocument(
		"_id", int32(1),
		"grp", int32(1),
		"tag", int32(1),
	))
	projStage, err := stages.NewStage(must.NotFail(types.NewDocument("$project", projSpec)))
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		closer := iterator.NewMultiCloser()
		in := iterator.Values(iterator.ForSlice(docs))
		out, err := projStage.Process(context.Background(), in, closer)
		if err != nil {
			b.Fatal(err)
		}
		drain(out)
		closer.Close()
	}
}

func BenchmarkStage_Sort(b *testing.B) {
	docs := makeBenchDocs()

	sortSpec := must.NotFail(types.NewDocument("i", int32(-1)))
	sortStage, err := stages.NewStage(must.NotFail(types.NewDocument("$sort", sortSpec)))
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		closer := iterator.NewMultiCloser()
		in := iterator.Values(iterator.ForSlice(docs))
		out, err := sortStage.Process(context.Background(), in, closer)
		if err != nil {
			b.Fatal(err)
		}
		drain(out)
		closer.Close()
	}
}
