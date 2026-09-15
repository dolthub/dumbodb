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

package oplog

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestFetcherProvesInclusiveContinuityAndSkipsDuplicate(t *testing.T) {
	manager := configuredFetcherManager(t, testFetchOpTime(1))
	buffer := must.NotFail(NewBuffer(BufferLimits{Entries: 10, Bytes: 1024 * 1024}))
	client := &fakeFetchClient{responses: []*wire.OpMsg{
		wire.MustOpMsg("rbid", int32(5), "ok", float64(1)),
		testOplogResponse(t, 0, 5, testOplogDocument(1, 8), testOplogDocument(2, 8)),
	}}
	fetcher := testFetcher(t, manager, buffer, client)
	if err := fetcher.Fetch(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Fetch error = %v, want EOF", err)
	}
	entries := buffer.Drain(10, 1024*1024)
	if len(entries) != 1 || entries[0].OpTime != testFetchOpTime(2) {
		t.Fatalf("buffered entries = %+v", entries)
	}
	state := manager.Snapshot()
	if state.Checkpoint.Fetched != testFetchOpTime(2) || state.Checkpoint.Buffered != testFetchOpTime(2) {
		t.Fatalf("fetch checkpoint = %+v", state.Checkpoint)
	}
}

func TestFetcherFetchFromUsesExplicitInitialSyncPosition(t *testing.T) {
	manager := configuredFetcherManager(t, testFetchOpTime(1))
	buffer := must.NotFail(NewBuffer(BufferLimits{Entries: 10, Bytes: 1024 * 1024}))
	client := &fakeFetchClient{responses: []*wire.OpMsg{
		wire.MustOpMsg("rbid", int32(5), "ok", float64(1)),
		testOplogResponse(t, 0, 5, testOplogDocument(5, 8), testOplogDocument(6, 8)),
	}}
	fetcher := testFetcher(t, manager, buffer, client)
	if err := fetcher.FetchFrom(context.Background(), testFetchOpTime(5)); !errors.Is(err, io.EOF) {
		t.Fatalf("FetchFrom error = %v, want EOF", err)
	}
	entries := buffer.Drain(10, 1024*1024)
	if len(entries) != 1 || entries[0].OpTime != testFetchOpTime(6) {
		t.Fatalf("buffered entries = %+v", entries)
	}
}

func TestFetcherClassifiesTruncatedAndDivergentContinuity(t *testing.T) {
	tests := []struct {
		name     string
		first    *types.Document
		expected error
	}{
		{name: "truncated", first: testOplogDocument(2, 8), expected: ErrTooStale},
		{name: "different term", first: testOplogDocument(1, 9), expected: ErrContinuityLost},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := configuredFetcherManager(t, testFetchOpTime(1))
			buffer := must.NotFail(NewBuffer(BufferLimits{Entries: 10, Bytes: 1024 * 1024}))
			client := &fakeFetchClient{responses: []*wire.OpMsg{
				wire.MustOpMsg("rbid", int32(5), "ok", float64(1)),
				testOplogResponse(t, 0, 5, test.first),
			}}
			fetcher := testFetcher(t, manager, buffer, client)
			if err := fetcher.Fetch(context.Background()); !errors.Is(err, test.expected) {
				t.Fatalf("Fetch error = %v, want %v", err, test.expected)
			}
		})
	}
}

func TestFetcherPausesAtBufferLimitAndResumesAfterDrain(t *testing.T) {
	manager := configuredFetcherManager(t, testFetchOpTime(1))
	buffer := must.NotFail(NewBuffer(BufferLimits{Entries: 1, Bytes: 1024 * 1024}))
	client := &fakeFetchClient{responses: []*wire.OpMsg{
		wire.MustOpMsg("rbid", int32(5), "ok", float64(1)),
		testOplogResponse(t, 0, 5, testOplogDocument(1, 8), testOplogDocument(2, 8), testOplogDocument(3, 8)),
	}}
	fetcher := testFetcher(t, manager, buffer, client)
	done := make(chan error, 1)
	go func() { done <- fetcher.Fetch(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for buffer.Stats().Entries != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := buffer.Stats(); stats.Entries != 1 {
		t.Fatalf("buffer did not fill: %+v", stats)
	}
	first := buffer.Drain(1, 1024*1024)
	if len(first) != 1 || first[0].OpTime != testFetchOpTime(2) {
		t.Fatalf("first drain = %+v", first)
	}
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Fetch error = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Fetch did not resume after buffer drain")
	}
	second := buffer.Drain(1, 1024*1024)
	if len(second) != 1 || second[0].OpTime != testFetchOpTime(3) {
		t.Fatalf("second drain = %+v", second)
	}
}

func TestFetcherRejectsRollbackIDChange(t *testing.T) {
	manager := configuredFetcherManager(t, testFetchOpTime(1))
	if err := manager.ObserveSourceRBID("primary.example:27017", 5); err != nil {
		t.Fatal(err)
	}
	buffer := must.NotFail(NewBuffer(BufferLimits{Entries: 10, Bytes: 1024 * 1024}))
	client := &fakeFetchClient{responses: []*wire.OpMsg{wire.MustOpMsg("rbid", int32(6), "ok", float64(1))}}
	fetcher := testFetcher(t, manager, buffer, client)
	if err := fetcher.Fetch(context.Background()); !errors.Is(err, topology.ErrSourceRollbackIDChanged) {
		t.Fatalf("Fetch error = %v", err)
	}
}

func TestFetcherRunTransitionsAfterContinuityFailure(t *testing.T) {
	tests := []struct {
		name     string
		first    *types.Document
		expected topology.MemberState
	}{
		{name: "truncated", first: testOplogDocument(2, 8), expected: topology.StateStartup2},
		{name: "divergent", first: testOplogDocument(1, 9), expected: topology.StateRecovering},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := configuredFetcherManager(t, testFetchOpTime(1))
			buffer := must.NotFail(NewBuffer(BufferLimits{Entries: 10, Bytes: 1024 * 1024}))
			client := &fakeFetchClient{responses: []*wire.OpMsg{
				wire.MustOpMsg("rbid", int32(5), "ok", float64(1)),
				testOplogResponse(t, 0, 5, test.first),
			}}
			fetcher := testFetcher(t, manager, buffer, client)
			if err := fetcher.Run(context.Background()); err == nil {
				t.Fatal("Run unexpectedly returned nil")
			}
			if state := manager.Snapshot().State; state != test.expected {
				t.Fatalf("member state = %s, want %s", state, test.expected)
			}
		})
	}
}

func TestFetcherStopsWhenSourceChanges(t *testing.T) {
	manager := configuredFetcherManager(t, testFetchOpTime(1))
	buffer := must.NotFail(NewBuffer(BufferLimits{Entries: 10, Bytes: 1024 * 1024}))
	client := &fakeFetchClient{responses: []*wire.OpMsg{
		wire.MustOpMsg("rbid", int32(5), "ok", float64(1)),
		testOplogResponse(t, 0, 5, testOplogDocument(1, 8), testOplogDocument(2, 8)),
	}}
	client.onRequest = func(request int) {
		if request == 2 {
			if err := manager.MarkMemberDown(1); err != nil {
				t.Error(err)
			}
		}
	}
	fetcher := testFetcher(t, manager, buffer, client)
	if err := fetcher.Fetch(context.Background()); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Fetch error = %v, want source changed", err)
	}
	if stats := buffer.Stats(); stats.Entries != 0 {
		t.Fatalf("buffer changed after source replacement: %+v", stats)
	}
}

type fakeFetchClient struct {
	responses []*wire.OpMsg
	err       error
	requests  int
	onRequest func(int)
}

func (c *fakeFetchClient) Request(context.Context, *wire.OpMsg) (*wire.OpMsg, error) {
	c.requests++
	if c.onRequest != nil {
		c.onRequest(c.requests)
	}
	if c.err != nil {
		return nil, c.err
	}
	if len(c.responses) == 0 {
		return nil, context.Canceled
	}
	response := c.responses[0]
	c.responses = c.responses[1:]
	return response, nil
}

func (c *fakeFetchClient) Exhaust(context.Context, *wire.OpMsg, func(*wire.OpMsg) (bool, error)) error {
	return errors.New("unexpected exhaust call")
}

func (c *fakeFetchClient) Close() error {
	return nil
}

func testFetcher(t *testing.T, manager *topology.Manager, buffer *Buffer, client fetchClient) *Fetcher {
	t.Helper()
	fetcher, err := NewFetcher(manager, buffer, nil)
	if err != nil {
		t.Fatal(err)
	}
	fetcher.exhaust = false
	fetcher.newClient = func(string, string) fetchClient { return client }
	return fetcher
}

func configuredFetcherManager(t *testing.T, checkpoint control.OpTime) *topology.Manager {
	t.Helper()
	store, err := control.Open(t.TempDir(), control.Configuration{
		SetName: "rs0", MemberHost: "dumbo.example:27017",
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := topology.New(store)
	configuration := control.ReplicaConfiguration{
		SetName: "rs0", Version: 4, Term: 3, ProtocolVersion: 1, ReplicaSetID: "0102030405060708090a0b0c",
		Members: []control.MemberConfiguration{
			{MemberID: 1, Host: "primary.example:27017", Priority: 1, Votes: 1},
			{MemberID: 3, Host: "dumbo.example:27017", Hidden: true, Priority: 0, Votes: 0},
		},
	}
	if err := manager.InstallConfiguration(configuration, 8); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveHeartbeat("primary.example:27017", topology.Heartbeat{
		SetName: "rs0", MemberID: 1, State: topology.StatePrimary, Term: 8, PrimaryID: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkInitialSyncComplete(control.Checkpoint{
		Fetched: checkpoint, Buffered: checkpoint, Written: checkpoint, Durable: checkpoint, Applied: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	return manager
}

func testOplogResponse(t *testing.T, cursorID, rbid int64, documents ...*types.Document) *wire.OpMsg {
	t.Helper()
	batch := types.MakeArray(len(documents))
	for _, document := range documents {
		batch.Append(document)
	}
	cursor := must.NotFail(types.NewDocument("id", cursorID, "ns", "local.oplog.rs", "firstBatch", batch))
	opTime := must.NotFail(types.NewDocument(
		"ts", types.Timestamp(uint64(100)<<32|20),
		"t", int64(8),
	))
	oplogMetadata := must.NotFail(types.NewDocument(
		"lastOpCommitted", opTime,
		"lastOpApplied", opTime,
		"lastOpWritten", opTime,
		"rbid", rbid,
		"primaryIndex", int32(0),
		"syncSourceIndex", int32(-1),
		"syncSourceHost", "",
	))
	var replicaSetID types.ObjectID
	copy(replicaSetID[:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	replMetadata := must.NotFail(types.NewDocument(
		"term", int64(8),
		"lastOpCommitted", opTime,
		"lastOpVisible", opTime,
		"configVersion", int64(4),
		"configTerm", int64(3),
		"replicaSetId", replicaSetID,
		"syncSourceIndex", int32(-1),
		"isPrimary", true,
	))
	wireCursor, err := bson.FromDocument(cursor)
	if err != nil {
		t.Fatal(err)
	}
	wireOplogMetadata, err := bson.FromDocument(oplogMetadata)
	if err != nil {
		t.Fatal(err)
	}
	wireReplMetadata, err := bson.FromDocument(replMetadata)
	if err != nil {
		t.Fatal(err)
	}
	return wire.MustOpMsg(
		"cursor", wireCursor,
		"$oplogQueryData", wireOplogMetadata,
		"$replData", wireReplMetadata,
		"ok", float64(1),
	)
}

func testOplogDocument(increment uint32, term int64) *types.Document {
	return must.NotFail(types.NewDocument(
		"ts", types.Timestamp(uint64(100)<<32|uint64(increment)),
		"t", term,
		"op", "n",
		"ns", "",
		"o", must.NotFail(types.NewDocument("msg", "test")),
	))
}

func testFetchOpTime(increment uint32) control.OpTime {
	return control.OpTime{Seconds: 100, Increment: increment, Term: 8}
}
