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

// Investigation tool for workspace-fes: walk the chunk store after a
// representative workload and break the byte total down by flatbuffer
// file-ID category. Answers "what does dumbo's chunk store actually
// look like after inserting N documents?" -- separating per-collection
// wrappers (DTBL, DSCH), address maps (ADRM), prolly leaves (TUPM),
// and the commit / working-set / root-value chunks from the document
// payload itself.
//
// Headline finding (canonical {_id, email, name, age} docs, post-GC):
//
//   N      TUPM    DTBL+DSCH+ADRM    DCMT+RTVL+WRST+CMCL    other
//   10k    99.70%   0.17%             0.12%                  0.00%
//   100k   99.82%   0.07%             0.11%                  0.00%
//
// The per-collection wrapper hypothesis that motivated this task
// (wrappers contributing meaningfully to the residual storage gap
// vs DoltJSON) is wrong at every scale we measured: wrappers are
// well under 1% of total chunk bytes. The dominant cost is TUPM
// (the prolly tree nodes that carry the row tuples themselves).
// Any future residual chasing should target TUPM-level efficiency,
// not the wrapper layer.

package dolt

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/stretchr/testify/require"
)

type chunkStat struct {
	count int
	bytes int64
}

func TestStorageDecomposition_Dumbo(t *testing.T) {
	if testing.Short() {
		t.Skip("storage-decomposition investigation; omit -short to run")
	}
	ctx := context.Background()

	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), true, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	dbName := "decompose"
	ctx = ctxWithSession(t, be, "test-lsid-decompose")

	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}))
	coll, err := db.Collection("items")
	require.NoError(t, err)

	const n = 10000
	docs := make([]*types.Document, 0, n)
	for i := 0; i < n; i++ {
		doc, mErr := types.NewDocument(
			"_id", fmt.Sprintf("doc%07d", i),
			"email", fmt.Sprintf("user%07d@example.com", i),
			"name", fmt.Sprintf("User %d", i),
			"age", int32(20+i%50),
		)
		require.NoError(t, mErr)
		docs = append(docs, doc)
	}

	const batchSize = 500
	for off := 0; off < n; off += batchSize {
		end := off + batchSize
		if end > n {
			end = n
		}
		_, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs[off:end]})
		require.NoError(t, err)
	}

	// InsertAll auto-commits each batch through the working set, so by
	// this point the chunks are persistent. Default-mode GC sweeps
	// new-gen plus unreferenced old-gen.
	_, err = be.DumboDBGC(ctx, &backends.GCParams{DBName: dbName, Mode: "default"})
	require.NoError(t, err)

	state, ok := be.lookupDbStateForDsess(dbName)
	require.True(t, ok)

	byType := make(map[string]*chunkStat)
	err = state.cs.IterateAllChunks(ctx, func(c chunks.Chunk) {
		id := classifyChunk(c)
		s := byType[id]
		if s == nil {
			s = &chunkStat{}
			byType[id] = s
		}
		s.count++
		s.bytes += int64(len(c.Data()))
	})
	require.NoError(t, err)

	printDecomposition(t, fmt.Sprintf("DumboDB, %d canonical docs, post-commit + default GC", n), byType)
}

// classifyChunk returns the human-readable category for c. Most chunks
// are flatbuffers and identified by their 4-byte file-ID. The handful
// of non-flatbuffer formats (raw noms blobs, etc.) get a fallback
// label so they're still visible in the breakdown.
func classifyChunk(c chunks.Chunk) string {
	data := c.Data()
	if len(data) < serial.MessagePrefixSz+8 {
		return "<too-short>"
	}
	id := serial.GetFileID(data)
	if id == "" {
		return "<no-fileid>"
	}
	// Sanity-check the file ID is 4 printable ASCII characters; if
	// not, the chunk likely isn't a serial flatbuffer.
	for _, r := range id {
		if r < 0x20 || r > 0x7E {
			return "<non-fb>"
		}
	}
	return id
}

func printDecomposition(t *testing.T, header string, byType map[string]*chunkStat) {
	t.Helper()

	keys := make([]string, 0, len(byType))
	for k := range byType {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return byType[keys[i]].bytes > byType[keys[j]].bytes
	})

	var totalBytes int64
	var totalCount int
	for _, k := range keys {
		totalBytes += byType[k].bytes
		totalCount += byType[k].count
	}

	t.Logf("=== %s ===", header)
	t.Logf("%-14s %10s %14s %8s", "Type", "Chunks", "Bytes", "Pct")
	t.Logf("--------------+-----------+----------------+--------")
	for _, k := range keys {
		s := byType[k]
		pct := 100.0 * float64(s.bytes) / float64(totalBytes)
		t.Logf("%-14s %10d %14d %7.2f%%", chunkLabel(k), s.count, s.bytes, pct)
	}
	t.Logf("--------------+-----------+----------------+--------")
	t.Logf("%-14s %10d %14d %7.2f%%", "TOTAL", totalCount, totalBytes, 100.0)
}

// chunkLabel maps the 4-byte file-ID to a human-readable label.
func chunkLabel(id string) string {
	switch id {
	case serial.StoreRootFileID:
		return "STRT (root)"
	case serial.CommitFileID:
		return "DCMT (commit)"
	case serial.RootValueFileID:
		return "RTVL (rootval)"
	case serial.WorkingSetFileID:
		return "WRST (wset)"
	case serial.TableFileID:
		return "DTBL (table)"
	case serial.TableSchemaFileID:
		return "DSCH (schema)"
	case serial.AddressMapFileID:
		return "ADRM (addrmap)"
	case serial.ProllyTreeNodeFileID:
		return "TUPM (prolly)"
	case serial.BlobFileID:
		return "BLOB"
	case serial.CommitClosureFileID:
		return "CMCL (commitcl)"
	case serial.MergeArtifactsFileID:
		return "ARTM (merge)"
	case serial.TagFileID:
		return "DTAG"
	default:
		return id
	}
}
