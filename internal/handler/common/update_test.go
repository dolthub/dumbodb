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
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestPipelineUpdate_replaceWith tests $replaceWith as a stage in an update pipeline.
// MongoDB allows UpdateOne/UpdateMany with a pipeline argument where $replaceWith
// replaces the entire document with the expression result.
func TestPipelineUpdate_replaceWith(t *testing.T) {
	t.Parallel()

	t.Run("field_path", func(t *testing.T) {
		t.Parallel()

		// Document: {_id: 1, a: 1, sub: {x: 10, y: 20}}
		// Pipeline: [{$replaceWith: "$sub"}]
		// Expected: {x: 10, y: 20}
		sub := must.NotFail(types.NewDocument("x", int32(10), "y", int32(20)))
		doc := must.NotFail(types.NewDocument("_id", int32(1), "a", int32(1), "sub", sub))

		stageDoc := must.NotFail(types.NewDocument("$replaceWith", "$sub"))
		pipeline := must.NotFail(types.NewArray(stageDoc))

		changed, err := processPipelineUpdate(doc, pipeline)
		if err != nil {
			t.Fatalf("processPipelineUpdate: %v", err)
		}

		if !changed {
			t.Fatal("expected document to be changed")
		}

		if doc.Has("_id") {
			t.Error("expected _id to be removed after $replaceWith")
		}

		if doc.Has("a") {
			t.Error("expected 'a' to be removed after $replaceWith")
		}

		xVal, err := doc.Get("x")
		if err != nil {
			t.Fatalf("expected 'x' in result: %v", err)
		}

		if xVal != int32(10) {
			t.Errorf("expected x=10, got %v", xVal)
		}

		yVal, err := doc.Get("y")
		if err != nil {
			t.Fatalf("expected 'y' in result: %v", err)
		}

		if yVal != int32(20) {
			t.Errorf("expected y=20, got %v", yVal)
		}
	})

	t.Run("literal_document", func(t *testing.T) {
		t.Parallel()

		// Document: {_id: 1, name: "alice", score: 42}
		// Pipeline: [{$replaceWith: {player: "$name", points: "$score"}}]
		// Expected: {player: "alice", points: 42}
		doc := must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "score", int32(42)))

		exprDoc := must.NotFail(types.NewDocument("player", "$name", "points", "$score"))
		stageDoc := must.NotFail(types.NewDocument("$replaceWith", exprDoc))
		pipeline := must.NotFail(types.NewArray(stageDoc))

		changed, err := processPipelineUpdate(doc, pipeline)
		if err != nil {
			t.Fatalf("processPipelineUpdate: %v", err)
		}

		if !changed {
			t.Fatal("expected document to be changed")
		}

		playerVal, err := doc.Get("player")
		if err != nil {
			t.Fatalf("expected 'player' in result: %v", err)
		}

		if playerVal != "alice" {
			t.Errorf("expected player='alice', got %v", playerVal)
		}

		pointsVal, err := doc.Get("points")
		if err != nil {
			t.Fatalf("expected 'points' in result: %v", err)
		}

		if pointsVal != int32(42) {
			t.Errorf("expected points=42, got %v", pointsVal)
		}
	})

	t.Run("no_change_when_same", func(t *testing.T) {
		t.Parallel()

		// Document: {x: 1, y: 2}
		// Pipeline: [{$replaceWith: {x: "$x", y: "$y"}}]
		// Result has same keys/values → changed should be false.
		doc := must.NotFail(types.NewDocument("x", int32(1), "y", int32(2)))

		exprDoc := must.NotFail(types.NewDocument("x", "$x", "y", "$y"))
		stageDoc := must.NotFail(types.NewDocument("$replaceWith", exprDoc))
		pipeline := must.NotFail(types.NewArray(stageDoc))

		changed, err := processPipelineUpdate(doc, pipeline)
		if err != nil {
			t.Fatalf("processPipelineUpdate: %v", err)
		}

		if changed {
			t.Fatal("expected document to be unchanged when replacement is equivalent")
		}
	})
}
