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
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestDiscoverBoundariesIncludesOldestActiveTransaction(t *testing.T) {
	sourceID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	client := &boundaryClient{responses: []*wire.OpMsg{
		responseMessage(t, must.NotFail(types.NewDocument("rbid", int32(17), "ok", float64(1)))),
		cursorResponse(t, oplogBoundaryDocument(10, 8)),
		cursorResponse(t, must.NotFail(types.NewDocument("startOpTime", opTimeDocument(5, 8)))),
		cursorResponse(t, oplogBoundaryDocument(12, 8)),
		cursorResponse(t, must.NotFail(types.NewDocument("_id", types.Binary{Subtype: types.BinaryUUID, B: sourceID[:]}))),
	}}
	attempt, err := DiscoverBoundaries(context.Background(), client, "mongo.example:27017")
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ID == "" || attempt.SourceRBID != 17 || attempt.Source != "mongo.example:27017" {
		t.Fatalf("attempt identity = %+v", attempt)
	}
	if attempt.BeginFetch != (control.OpTime{Seconds: 100, Increment: 5, Term: 8}) {
		t.Fatalf("begin-fetch = %+v", attempt.BeginFetch)
	}
	if attempt.BeginApply != (control.OpTime{Seconds: 100, Increment: 12, Term: 8}) {
		t.Fatalf("begin-apply = %+v", attempt.BeginApply)
	}
	if attempt.SourceInitialSyncID != sourceID.String() {
		t.Fatalf("source initial sync ID = %q", attempt.SourceInitialSyncID)
	}
	if len(client.requests) != 5 {
		t.Fatalf("request count = %d", len(client.requests))
	}
	assertCommand(t, client.requests[0], "replSetGetRBID", "admin")
	assertCommand(t, client.requests[1], "find", "local")
	assertCommand(t, client.requests[2], "find", "config")
	assertCommand(t, client.requests[3], "find", "local")
	assertCommand(t, client.requests[4], "find", "local")
}

func TestBoundaryStopAndRollbackValidation(t *testing.T) {
	client := &boundaryClient{responses: []*wire.OpMsg{
		cursorResponse(t, oplogBoundaryDocument(20, 9)),
		responseMessage(t, must.NotFail(types.NewDocument("rbid", int64(4), "ok", float64(1)))),
		responseMessage(t, must.NotFail(types.NewDocument("rbid", int32(5), "ok", float64(1)))),
	}}
	begin := control.OpTime{Seconds: 100, Increment: 12, Term: 8}
	stop, err := ReadStopPosition(context.Background(), client, begin)
	if err != nil {
		t.Fatal(err)
	}
	if stop != (control.OpTime{Seconds: 100, Increment: 20, Term: 9}) {
		t.Fatalf("stop = %+v", stop)
	}
	if err := VerifyRollbackID(context.Background(), client, 4); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRollbackID(context.Background(), client, 4); err == nil {
		t.Fatal("accepted changed rollback ID")
	}
}

type boundaryClient struct {
	requests  []*wire.OpMsg
	responses []*wire.OpMsg
}

func (c *boundaryClient) Request(_ context.Context, request *wire.OpMsg) (*wire.OpMsg, error) {
	c.requests = append(c.requests, request)
	response := c.responses[0]
	c.responses = c.responses[1:]
	return response, nil
}

func responseMessage(t *testing.T, document *types.Document) *wire.OpMsg {
	t.Helper()
	message, err := wire.NewOpMsg(must.NotFail(bson.FromDocument(document)))
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func cursorResponse(t *testing.T, documents ...*types.Document) *wire.OpMsg {
	t.Helper()
	values := make([]any, len(documents))
	for i, document := range documents {
		values[i] = document
	}
	batch := must.NotFail(types.NewArray(values...))
	cursor := must.NotFail(types.NewDocument("firstBatch", batch, "id", int64(0), "ns", "test.collection"))
	return responseMessage(t, must.NotFail(types.NewDocument("cursor", cursor, "ok", float64(1))))
}

func oplogBoundaryDocument(increment uint32, term int64) *types.Document {
	document := opTimeDocument(increment, term)
	document.Set("op", "n")
	document.Set("ns", "")
	document.Set("o", must.NotFail(types.NewDocument("msg", "boundary")))
	return document
}

func opTimeDocument(increment uint32, term int64) *types.Document {
	return must.NotFail(types.NewDocument("ts", types.Timestamp(uint64(100)<<32|uint64(increment)), "t", term))
}

func assertCommand(t *testing.T, message *wire.OpMsg, commandName, databaseName string) {
	t.Helper()
	raw, err := message.RawDocument()
	if err != nil {
		t.Fatal(err)
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := document.Get(commandName); err != nil || value == nil {
		t.Fatalf("request has no %s field: %v", commandName, document)
	}
	if database, _ := document.Get("$db"); database != databaseName {
		t.Fatalf("request database = %v, want %q", database, databaseName)
	}
}
