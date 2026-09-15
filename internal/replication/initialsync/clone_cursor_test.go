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

package initialsync

import (
	"context"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCloneDocumentsUsesUUIDCursorAndResumeTokens(t *testing.T) {
	sourceUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	initialToken := must.NotFail(types.NewDocument("$recordId", int64(4), "$initialSyncId", types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]}))
	firstToken := must.NotFail(types.NewDocument("$recordId", int64(6), "$initialSyncId", types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]}))
	finalToken := must.NotFail(types.NewDocument("$recordId", int64(7), "$initialSyncId", types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]}))
	client := &boundaryClient{responses: []*wire.OpMsg{
		cloneCursorResponse(t, "firstBatch", 91, "orders.items", firstToken,
			must.NotFail(types.NewDocument("_id", int32(5))),
			must.NotFail(types.NewDocument("_id", int32(6))),
		),
		cloneCursorResponse(t, "nextBatch", 0, "orders.items", finalToken,
			must.NotFail(types.NewDocument("_id", int32(7))),
		),
	}}
	var ids []int32
	token, err := CloneDocuments(context.Background(), client, CloneCursor{
		Database: "orders", SourceUUID: types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]}, ResumeAfter: initialToken,
	}, func(document *types.Document) error {
		id, _ := document.Get("_id")
		ids = append(ids, id.(int32))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 5 || ids[1] != 6 || ids[2] != 7 {
		t.Fatalf("cloned IDs = %v", ids)
	}
	if types.Compare(token, finalToken) != types.Equal {
		t.Fatalf("final token = %v, want %v", token, finalToken)
	}
	if len(client.requests) != 2 {
		t.Fatalf("request count = %d", len(client.requests))
	}
	firstRequest := decodeRequest(t, client.requests[0])
	findValue, _ := firstRequest.Get("find")
	findUUID, ok := findValue.(types.Binary)
	if !ok || findUUID.Subtype != types.BinaryUUID || string(findUUID.B) != string(sourceUUID[:]) {
		t.Fatalf("find UUID = %v", findValue)
	}
	resumeAfter, _ := firstRequest.Get("resumeAfter")
	if types.Compare(resumeAfter, initialToken) != types.Equal {
		t.Fatalf("resumeAfter = %v, want %v", resumeAfter, initialToken)
	}
	secondRequest := decodeRequest(t, client.requests[1])
	if getMore, _ := secondRequest.Get("getMore"); getMore != int64(91) {
		t.Fatalf("getMore = %v", getMore)
	}
	if collection, _ := secondRequest.Get("collection"); collection != "items" {
		t.Fatalf("getMore collection = %v", collection)
	}
}

func TestCloneDocumentsReturnsLastCompleteBatchTokenOnConsumerFailure(t *testing.T) {
	sourceUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	previousToken := must.NotFail(types.NewDocument("$recordId", int64(3)))
	nextToken := must.NotFail(types.NewDocument("$recordId", int64(4)))
	client := &boundaryClient{responses: []*wire.OpMsg{
		cloneCursorResponse(t, "firstBatch", 0, "orders.items", nextToken, must.NotFail(types.NewDocument("_id", int32(4)))),
	}}
	wantErr := context.Canceled
	token, err := CloneDocuments(context.Background(), client, CloneCursor{
		Database: "orders", SourceUUID: types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]}, ResumeAfter: previousToken,
	}, func(*types.Document) error { return wantErr })
	if err != wantErr {
		t.Fatalf("clone error = %v, want %v", err, wantErr)
	}
	if types.Compare(token, previousToken) != types.Equal {
		t.Fatalf("failure token = %v, want %v", token, previousToken)
	}
}

func cloneCursorResponse(t *testing.T, batchName string, cursorID int64, namespace string, token *types.Document, documents ...*types.Document) *wire.OpMsg {
	t.Helper()
	values := make([]any, len(documents))
	for index, document := range documents {
		values[index] = document
	}
	cursor := must.NotFail(types.NewDocument(
		batchName, must.NotFail(types.NewArray(values...)),
		"postBatchResumeToken", token,
		"id", cursorID,
		"ns", namespace,
	))
	return responseMessage(t, must.NotFail(types.NewDocument("cursor", cursor, "ok", float64(1))))
}

func decodeRequest(t *testing.T, message *wire.OpMsg) *types.Document {
	t.Helper()
	raw, err := message.RawDocument()
	if err != nil {
		t.Fatal(err)
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	return document
}
