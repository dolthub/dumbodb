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

package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/clientconn/cursor"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestExplainStatsPipelineCostsOneRun: explain at executionStats of a
// $collStats pipeline runs it once, so it costs about what the aggregate
// does rather than growing with the square of its stages.
func TestExplainStatsPipelineCostsOneRun(t *testing.T) {
	h := authGateHandler(t, false)
	h.cursors = cursor.NewRegistry(h.L)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	db, err := h.b.Database("x")
	require.NoError(t, err)
	docs := make([]*types.Document, 2000)
	for i := range docs {
		docs[i] = must.NotFail(types.NewDocument("_id", int32(i), "pad", strings.Repeat("x", 80)))
	}
	for _, name := range []string{"c", "big"} {
		require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: name}))
		c, err := db.Collection(name)
		require.NoError(t, err)
		_, err = c.InsertAll(ctx, &backends.InsertAllParams{Docs: docs})
		require.NoError(t, err)
	}

	pipeline := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("$collStats", must.NotFail(types.NewDocument("count", types.MakeDocument(0)))))))
	for range 60 {
		pipeline.Append(must.NotFail(types.NewDocument("$unionWith", "big")))
	}
	aggregate := func() *types.Document {
		return must.NotFail(types.NewDocument("aggregate", "c", "pipeline", pipeline, "cursor", must.NotFail(types.NewDocument("batchSize", int32(0)))))
	}

	// The yardstick runs the whole pipeline: a trailing $count makes the
	// first batch drain it, as explain's execution does.
	counted := pipeline.DeepCopy()
	counted.Append(must.NotFail(types.NewDocument("$count", "n")))
	start := time.Now()
	agg := must.NotFail(types.NewDocument("aggregate", "c", "pipeline", counted, "cursor", types.MakeDocument(0)))
	agg.Set("$db", "x")
	_, err = h.commands["aggregate"].Handler(ctx, must.NotFail(documentOpMsg(agg)))
	require.NoError(t, err)
	aggregateTook := time.Since(start)

	start = time.Now()
	_, err = h.commands["explain"].Handler(ctx, explainMsg(t, "x", aggregate()))
	require.NoError(t, err)
	explainTook := time.Since(start)

	t.Logf("aggregate %v explain %v", aggregateTook, explainTook)
	require.Less(t, explainTook, 5*aggregateTook+2*time.Second,
		"explain took %v, the aggregate %v", explainTook, aggregateTook)
}
