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
	"errors"
	"fmt"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
)

type requestClient interface {
	Request(context.Context, *wire.OpMsg) (*wire.OpMsg, error)
}

func DiscoverBoundaries(ctx context.Context, client requestClient, source string) (control.InitialSyncAttempt, error) {
	if client == nil || source == "" {
		return control.InitialSyncAttempt{}, errors.New("initial sync boundary discovery requires client and source")
	}
	rbid, err := rollbackID(ctx, client)
	if err != nil {
		return control.InitialSyncAttempt{}, err
	}
	beginFetch, err := newestOplogEntry(ctx, client)
	if err != nil {
		return control.InitialSyncAttempt{}, err
	}
	transactionFloor, found, err := oldestActiveTransaction(ctx, client, beginFetch)
	if err != nil {
		return control.InitialSyncAttempt{}, err
	}
	if found && transactionFloor.Compare(beginFetch) < 0 {
		beginFetch = transactionFloor
	}
	beginApply, err := newestOplogEntry(ctx, client)
	if err != nil {
		return control.InitialSyncAttempt{}, err
	}
	if beginApply.Compare(beginFetch) < 0 {
		return control.InitialSyncAttempt{}, fmt.Errorf("source oplog moved backwards from %v to %v", beginFetch, beginApply)
	}
	sourceInitialSyncID, err := sourceInitialSyncID(ctx, client)
	if err != nil {
		return control.InitialSyncAttempt{}, err
	}
	return control.InitialSyncAttempt{
		ID: "initial-sync:" + uuid.NewString(), Source: source, SourceRBID: rbid,
		SourceInitialSyncID: sourceInitialSyncID, BeginFetch: beginFetch, BeginApply: beginApply,
	}, nil
}

func ReadStopPosition(ctx context.Context, client requestClient, beginApply control.OpTime) (control.OpTime, error) {
	stop, err := newestOplogEntry(ctx, client)
	if err != nil {
		return control.OpTime{}, err
	}
	if stop.Compare(beginApply) < 0 {
		return control.OpTime{}, fmt.Errorf("initial sync stop position %v precedes begin-apply %v", stop, beginApply)
	}
	return stop, nil
}

func VerifyRollbackID(ctx context.Context, client requestClient, expected int64) error {
	actual, err := rollbackID(ctx, client)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("initial sync source rollback ID changed from %d to %d", expected, actual)
	}
	return nil
}

func rollbackID(ctx context.Context, client requestClient) (int64, error) {
	document, err := requestDocument(ctx, client, wire.MustOpMsg("replSetGetRBID", int32(1), "$replData", int32(1), "$db", "admin"))
	if err != nil {
		return 0, err
	}
	value, _ := document.Get("rbid")
	rbid, err := integer(value)
	if err != nil || rbid < 0 {
		return 0, fmt.Errorf("replSetGetRBID rbid has invalid value %v", value)
	}
	return rbid, nil
}

func newestOplogEntry(ctx context.Context, client requestClient) (control.OpTime, error) {
	sort := wirebson.MakeDocument(1)
	_ = sort.Add("$natural", int32(-1))
	readConcern := wirebson.MakeDocument(1)
	_ = readConcern.Add("level", "local")
	readPreference := wirebson.MakeDocument(1)
	_ = readPreference.Add("mode", "secondaryPreferred")
	document, err := requestDocument(ctx, client, wire.MustOpMsg(
		"find", "oplog.rs", "sort", sort, "limit", int32(1),
		"readConcern", readConcern, "$readPreference", readPreference, "$db", "local",
	))
	if err != nil {
		return control.OpTime{}, err
	}
	batch, err := firstBatch(document)
	if err != nil {
		return control.OpTime{}, err
	}
	if batch.Len() != 1 {
		return control.OpTime{}, fmt.Errorf("newest oplog query returned %d entries, want 1", batch.Len())
	}
	value, _ := batch.Get(0)
	entry, ok := value.(*types.Document)
	if !ok {
		return control.OpTime{}, fmt.Errorf("newest oplog entry has type %T, want document", value)
	}
	return parseOpTimeFields(entry)
}

func oldestActiveTransaction(ctx context.Context, client requestClient, after control.OpTime) (control.OpTime, bool, error) {
	states := wirebson.MakeArray(2)
	_ = states.Add("prepared")
	_ = states.Add("inProgress")
	in := wirebson.MakeDocument(1)
	_ = in.Add("$in", states)
	filter := wirebson.MakeDocument(1)
	_ = filter.Add("state", in)
	sort := wirebson.MakeDocument(1)
	_ = sort.Add("startOpTime", int32(1))
	readConcern := wirebson.MakeDocument(2)
	_ = readConcern.Add("level", "local")
	_ = readConcern.Add("afterClusterTime", wirebson.Timestamp(uint64(after.Seconds)<<32|uint64(after.Increment)))
	readPreference := wirebson.MakeDocument(1)
	_ = readPreference.Add("mode", "secondaryPreferred")
	document, err := requestDocument(ctx, client, wire.MustOpMsg(
		"find", "transactions", "filter", filter, "sort", sort, "readConcern", readConcern,
		"limit", int32(1), "$readPreference", readPreference, "$db", "config",
	))
	if err != nil {
		return control.OpTime{}, false, err
	}
	batch, err := firstBatch(document)
	if err != nil {
		return control.OpTime{}, false, err
	}
	if batch.Len() == 0 {
		return control.OpTime{}, false, nil
	}
	if batch.Len() != 1 {
		return control.OpTime{}, false, fmt.Errorf("active transaction query returned %d entries, want at most 1", batch.Len())
	}
	value, _ := batch.Get(0)
	transaction, ok := value.(*types.Document)
	if !ok {
		return control.OpTime{}, false, fmt.Errorf("active transaction has type %T, want document", value)
	}
	startValue, _ := transaction.Get("startOpTime")
	start, ok := startValue.(*types.Document)
	if !ok {
		return control.OpTime{}, false, fmt.Errorf("active transaction startOpTime has type %T, want document", startValue)
	}
	opTime, err := parseOpTimeFields(start)
	return opTime, true, err
}

func sourceInitialSyncID(ctx context.Context, client requestClient) (string, error) {
	readConcern := wirebson.MakeDocument(0)
	document, err := requestDocument(ctx, client, wire.MustOpMsg(
		"find", "replset.initialSyncId", "limit", int64(1), "readConcern", readConcern, "$db", "local",
	))
	if err != nil {
		return "", err
	}
	batch, err := firstBatch(document)
	if err != nil {
		return "", err
	}
	if batch.Len() == 0 {
		return "", nil
	}
	if batch.Len() != 1 {
		return "", fmt.Errorf("source initial sync ID query returned %d entries, want at most 1", batch.Len())
	}
	value, _ := batch.Get(0)
	record, ok := value.(*types.Document)
	if !ok {
		return "", fmt.Errorf("source initial sync ID record has type %T, want document", value)
	}
	idValue, _ := record.Get("_id")
	id, ok := idValue.(types.Binary)
	if !ok || id.Subtype != types.BinaryUUID || len(id.B) != 16 {
		return "", fmt.Errorf("source initial sync ID has type %T, want UUID binary", idValue)
	}
	parsed, err := uuid.FromBytes(id.B)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func requestDocument(ctx context.Context, client requestClient, request *wire.OpMsg) (*types.Document, error) {
	response, err := client.Request(ctx, request)
	if err != nil {
		return nil, err
	}
	raw, err := response.RawDocument()
	if err != nil {
		return nil, err
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		return nil, err
	}
	okValue, _ := document.Get("ok")
	if ok, valid := okValue.(float64); !valid || ok != 1 {
		message, _ := document.Get("errmsg")
		return nil, fmt.Errorf("MongoDB initial sync command failed: %v", message)
	}
	return document, nil
}

func firstBatch(document *types.Document) (*types.Array, error) {
	cursorValue, _ := document.Get("cursor")
	cursor, ok := cursorValue.(*types.Document)
	if !ok {
		return nil, fmt.Errorf("response cursor has type %T, want document", cursorValue)
	}
	batchValue, _ := cursor.Get("firstBatch")
	batch, ok := batchValue.(*types.Array)
	if !ok {
		return nil, fmt.Errorf("response firstBatch has type %T, want array", batchValue)
	}
	return batch, nil
}

func parseOpTimeFields(document *types.Document) (control.OpTime, error) {
	timestampValue, _ := document.Get("ts")
	timestamp, ok := timestampValue.(types.Timestamp)
	if !ok {
		return control.OpTime{}, fmt.Errorf("opTime ts has type %T, want timestamp", timestampValue)
	}
	termValue, _ := document.Get("t")
	term, err := integer(termValue)
	if err != nil {
		return control.OpTime{}, fmt.Errorf("opTime term: %w", err)
	}
	return control.OpTime{Seconds: uint32(uint64(timestamp) >> 32), Increment: uint32(timestamp), Term: term}, nil
}

func integer(value any) (int64, error) {
	switch value := value.(type) {
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	default:
		return 0, fmt.Errorf("has type %T, want integer", value)
	}
}
