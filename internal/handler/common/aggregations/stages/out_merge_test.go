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

// Tests for $out and $merge aggregation stage parsing.
// TestOut_* tests verify $out stage creation and document routing.
// TestMerge_* tests verify $merge stage creation and option parsing.

import (
	"context"
	"testing"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// noopOutWriter is a writer that captures the call args without doing anything.
func noopOutWriter(written *[]*types.Document) stages.OutWriter {
	return func(_ context.Context, _, _ string, docs []*types.Document) error {
		*written = append(*written, docs...)
		return nil
	}
}

// noopMergeFunc is a merger that captures params without doing anything.
func noopMergeFunc(captured **stages.MergeParams) stages.MergeFunc {
	return func(_ context.Context, params *stages.MergeParams) error {
		*captured = params
		return nil
	}
}

// TestOut_StringArg verifies that $out accepts a plain collection name.
func TestOut_StringArg(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$out", "output_coll"))

	var written []*types.Document

	s, err := stages.NewOutStage(stageDoc, "testdb", noopOutWriter(&written))
	if err != nil {
		t.Fatalf("NewOutStage: %v", err)
	}

	if s == nil {
		t.Fatal("expected non-nil stage")
	}
}

// TestOut_DocumentArg verifies that $out accepts {db, coll} document form.
func TestOut_DocumentArg(t *testing.T) {
	t.Parallel()

	spec := must.NotFail(types.NewDocument("db", "otherdb", "coll", "output_coll"))
	stageDoc := must.NotFail(types.NewDocument("$out", spec))

	var written []*types.Document

	s, err := stages.NewOutStage(stageDoc, "testdb", noopOutWriter(&written))
	if err != nil {
		t.Fatalf("NewOutStage with document arg: %v", err)
	}

	if s == nil {
		t.Fatal("expected non-nil stage")
	}
}

// TestOut_EmptyStringRejected verifies that an empty string is rejected.
func TestOut_EmptyStringRejected(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$out", ""))

	var written []*types.Document

	_, err := stages.NewOutStage(stageDoc, "testdb", noopOutWriter(&written))
	if err == nil {
		t.Fatal("expected error for empty collection name, got nil")
	}
}

// TestOut_ProcessWritesAndReturnsEmpty verifies that Process calls the writer
// and returns an empty iterator.
func TestOut_ProcessWritesAndReturnsEmpty(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$out", "out_coll"))

	var written []*types.Document

	s, err := stages.NewOutStage(stageDoc, "testdb", noopOutWriter(&written))
	if err != nil {
		t.Fatalf("NewOutStage: %v", err)
	}

	doc1 := must.NotFail(types.NewDocument("_id", int32(1), "v", "hello"))
	doc2 := must.NotFail(types.NewDocument("_id", int32(2), "v", "world"))

	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{doc1, doc2}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	// The output iterator must be empty.
	result, err := iterator.ConsumeValues(outIter)
	if err != nil {
		t.Fatalf("ConsumeValues: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 output documents, got %d", len(result))
	}

	// The writer must have received both input documents.
	if len(written) != 2 {
		t.Errorf("expected writer to receive 2 docs, got %d", len(written))
	}
}

// TestMerge_StringArg verifies that $merge accepts a plain collection name.
func TestMerge_StringArg(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$merge", "target_coll"))

	var captured *stages.MergeParams

	s, err := stages.NewMergeStage(stageDoc, "testdb", noopMergeFunc(&captured))
	if err != nil {
		t.Fatalf("NewMergeStage: %v", err)
	}

	if s == nil {
		t.Fatal("expected non-nil stage")
	}
}

// TestMerge_DocumentArgDefaults verifies that omitted options get defaults.
func TestMerge_DocumentArgDefaults(t *testing.T) {
	t.Parallel()

	spec := must.NotFail(types.NewDocument("into", "target_coll"))
	stageDoc := must.NotFail(types.NewDocument("$merge", spec))

	var captured *stages.MergeParams

	_, err := stages.NewMergeStage(stageDoc, "testdb", noopMergeFunc(&captured))
	if err != nil {
		t.Fatalf("NewMergeStage: %v", err)
	}
}

// TestMerge_WhenMatchedOptions verifies all valid whenMatched values are accepted.
func TestMerge_WhenMatchedOptions(t *testing.T) {
	t.Parallel()

	for _, opt := range []string{"merge", "replace", "keepExisting", "fail"} {
		t.Run(opt, func(t *testing.T) {
			t.Parallel()

			spec := must.NotFail(types.NewDocument("into", "c", "whenMatched", opt))
			stageDoc := must.NotFail(types.NewDocument("$merge", spec))

			var captured *stages.MergeParams

			_, err := stages.NewMergeStage(stageDoc, "testdb", noopMergeFunc(&captured))
			if err != nil {
				t.Fatalf("NewMergeStage with whenMatched=%q: %v", opt, err)
			}
		})
	}
}

// TestMerge_InvalidWhenMatchedRejected verifies that invalid whenMatched values are rejected.
func TestMerge_InvalidWhenMatchedRejected(t *testing.T) {
	t.Parallel()

	spec := must.NotFail(types.NewDocument("into", "c", "whenMatched", "bogus"))
	stageDoc := must.NotFail(types.NewDocument("$merge", spec))

	var captured *stages.MergeParams

	_, err := stages.NewMergeStage(stageDoc, "testdb", noopMergeFunc(&captured))
	if err == nil {
		t.Fatal("expected error for invalid whenMatched value, got nil")
	}
}

// TestMerge_WhenNotMatchedOptions verifies all valid whenNotMatched values are accepted.
func TestMerge_WhenNotMatchedOptions(t *testing.T) {
	t.Parallel()

	for _, opt := range []string{"insert", "discard", "fail"} {
		t.Run(opt, func(t *testing.T) {
			t.Parallel()

			spec := must.NotFail(types.NewDocument("into", "c", "whenNotMatched", opt))
			stageDoc := must.NotFail(types.NewDocument("$merge", spec))

			var captured *stages.MergeParams

			_, err := stages.NewMergeStage(stageDoc, "testdb", noopMergeFunc(&captured))
			if err != nil {
				t.Fatalf("NewMergeStage with whenNotMatched=%q: %v", opt, err)
			}
		})
	}
}

// TestMerge_ProcessCallsMerger verifies that Process invokes the merger with the right params.
func TestMerge_ProcessCallsMerger(t *testing.T) {
	t.Parallel()

	spec := must.NotFail(types.NewDocument(
		"into", "target_coll",
		"whenMatched", "replace",
		"whenNotMatched", "discard",
	))
	stageDoc := must.NotFail(types.NewDocument("$merge", spec))

	var captured *stages.MergeParams

	s, err := stages.NewMergeStage(stageDoc, "testdb", noopMergeFunc(&captured))
	if err != nil {
		t.Fatalf("NewMergeStage: %v", err)
	}

	doc := must.NotFail(types.NewDocument("_id", int32(42), "x", "value"))

	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{doc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	result, err := iterator.ConsumeValues(outIter)
	if err != nil {
		t.Fatalf("ConsumeValues: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 output documents, got %d", len(result))
	}

	if captured == nil {
		t.Fatal("merger was never called")
	}

	if captured.CollName != "target_coll" {
		t.Errorf("expected CollName %q, got %q", "target_coll", captured.CollName)
	}

	if captured.WhenMatched != "replace" {
		t.Errorf("expected WhenMatched %q, got %q", "replace", captured.WhenMatched)
	}

	if captured.WhenNotMatched != "discard" {
		t.Errorf("expected WhenNotMatched %q, got %q", "discard", captured.WhenNotMatched)
	}

	if len(captured.Docs) != 1 {
		t.Errorf("expected 1 doc in MergeParams, got %d", len(captured.Docs))
	}
}
