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

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// bigInput is ~130MB of documents by BSON size. They share one string, so the
// test itself stays small while the stage accounts each document in full.
func bigInput() []*types.Document {
	blob := strings.Repeat("x", 100*1024)
	docs := make([]*types.Document, 1300)
	for i := range docs {
		docs[i] = must.NotFail(types.NewDocument("_id", int32(i), "k", int32(i%7), "blob", blob))
	}
	return docs
}

func runStage(t *testing.T, spec *types.Document) error {
	t.Helper()
	stage, err := stages.NewStage(spec)
	require.NoError(t, err)
	closer := iterator.NewMultiCloser()
	defer closer.Close()
	out, err := stage.Process(context.Background(), iterator.Values(iterator.ForSlice(bigInput())), closer)
	if err != nil {
		return err
	}
	_, err = iterator.ConsumeValues(out)
	return err
}

// Before the fix blocking stages buffered their entire input with no limit
// (allowDiskUse was ignored), so one aggregate over a large collection could
// exhaust server memory. As in MongoDB, a blocking stage may use 100MB;
// DumboDB cannot spill to disk, so it fails with
// QueryExceededMemoryLimitNoDiskUseAllowed (292).
func TestBlockingStagesHaveMemoryLimit(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }

	for name, spec := range map[string]*types.Document{
		"$sort":       doc("$sort", doc("k", int32(1))),
		"$bucketAuto": doc("$bucketAuto", doc("groupBy", "$k", "buckets", int32(2))),
		"$facet":      doc("$facet", doc("a", must.NotFail(types.NewArray(doc("$sort", doc("k", int32(1))))))),
	} {
		err := runStage(t, spec)
		var cmdErr *handlererrors.CommandError
		require.True(t, errors.As(err, &cmdErr), "%s: want CommandError, got %v", name, err)
		require.Equal(t, handlererrors.ErrQueryExceededMemoryLimitNoDiskUseAllowed, cmdErr.Code(), "%s: %v", name, err)
	}

	// An accumulator's own 100MB limit on the values it keeps is reached
	// first, as in mongod, which can spill the stage but not the array.
	err := runStage(t, doc("$group", doc("_id", types.Null, "all", doc("$push", "$$ROOT"))))
	var cmdErr *handlererrors.CommandError
	require.True(t, errors.As(err, &cmdErr), "want CommandError, got %v", err)
	require.Equal(t, handlererrors.ErrExceededMemoryLimit, cmdErr.Code(), "%v", err)

	require.NoError(t, runStage(t, doc("$group", doc("_id", "$k", "n", doc("$sum", int32(1))))),
		"$group without retaining accumulators streams and stays within the limit")
	require.NoError(t, runStage(t, doc("$match", doc("k", int32(1)))), "streaming stages are not limited")
}

// bigLocatedInput is bigInput with a point on every document.
func bigLocatedInput() []*types.Document {
	docs := bigInput()
	for i, doc := range docs {
		doc.Set("loc", must.NotFail(types.NewArray(float64(i%10), float64(0))))
	}
	return docs
}

// $geoNear sorts by distance. MongoDB streams it from a geo index; DumboDB
// sorts in memory, so it is limited like $sort, counting only the documents
// it keeps.
func TestGeoNearHasMemoryLimit(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	near := func(extra ...any) *types.Document {
		spec := doc(append([]any{
			"near", must.NotFail(types.NewArray(float64(0), float64(0))),
			"distanceField", "dist",
			"key", "loc",
		}, extra...)...)
		return doc("$geoNear", spec)
	}
	run := func(spec *types.Document) error {
		stage, err := stages.NewStage(spec)
		require.NoError(t, err)
		closer := iterator.NewMultiCloser()
		defer closer.Close()
		out, err := stage.Process(context.Background(), iterator.Values(iterator.ForSlice(bigLocatedInput())), closer)
		if err != nil {
			return err
		}
		_, err = iterator.ConsumeValues(out)
		return err
	}

	err := run(near())
	var cmdErr *handlererrors.CommandError
	require.True(t, errors.As(err, &cmdErr), "want CommandError, got %v", err)
	require.Equal(t, handlererrors.ErrQueryExceededMemoryLimitNoDiskUseAllowed, cmdErr.Code())

	require.NoError(t, run(near("query", doc("k", int32(1)))), "documents the query rejects are not held")
}
