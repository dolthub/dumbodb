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

package topology

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
)

func TestProgressReportUsesOnlyDurableCheckpoint(t *testing.T) {
	state := positionReporterState()
	message := progressReportRequest(state)
	raw, err := message.RawDocument()
	if err != nil {
		t.Fatal(err)
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	positionsValue, _ := document.Get("optimes")
	positions, ok := positionsValue.(*types.Array)
	if !ok || positions.Len() != 1 {
		t.Fatalf("optimes = %T %+v", positionsValue, positionsValue)
	}
	positionValue, err := positions.Get(0)
	if err != nil {
		t.Fatal(err)
	}
	position, ok := positionValue.(*types.Document)
	if !ok {
		t.Fatalf("position = %T, want document", positionValue)
	}
	assertReportedOpTime(t, position, "writtenOpTime", state.Checkpoint.Written)
	assertReportedOpTime(t, position, "durableOpTime", state.Checkpoint.Durable)
	assertReportedOpTime(t, position, "appliedOpTime", state.Checkpoint.Applied)
	if value, _ := position.Get("memberId"); value != int32(state.MemberID) {
		t.Fatalf("memberId = %v, want %d", value, state.MemberID)
	}
	if value, _ := position.Get("cfgver"); value != state.Configuration.Version {
		t.Fatalf("cfgver = %v, want %d", value, state.Configuration.Version)
	}
}

func TestProgressReportForwardsHealthyMemberPositionsInIDOrder(t *testing.T) {
	state := positionReporterState()
	state.Members[7] = MemberStatus{MemberID: 7, Healthy: true, Written: control.OpTime{Seconds: 90, Increment: 7, Term: 2}, Durable: control.OpTime{Seconds: 90, Increment: 6, Term: 2}, Applied: control.OpTime{Seconds: 90, Increment: 5, Term: 2}}
	state.Members[2] = MemberStatus{MemberID: 2, Healthy: true, Written: control.OpTime{Seconds: 90, Increment: 4, Term: 2}, Durable: control.OpTime{Seconds: 90, Increment: 3, Term: 2}, Applied: control.OpTime{Seconds: 90, Increment: 2, Term: 2}}
	state.Members[9] = MemberStatus{MemberID: 9, Healthy: false}
	raw, err := progressReportRequest(state).RawDocument()
	if err != nil {
		t.Fatal(err)
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	positionsValue, _ := document.Get("optimes")
	positions := positionsValue.(*types.Array)
	if positions.Len() != 3 {
		t.Fatalf("optimes length = %d, want 3", positions.Len())
	}
	wantIDs := []int32{int32(state.MemberID), 2, 7}
	for index, wantID := range wantIDs {
		value, err := positions.Get(index)
		if err != nil {
			t.Fatal(err)
		}
		position := value.(*types.Document)
		if memberID, _ := position.Get("memberId"); memberID != wantID {
			t.Fatalf("optimes[%d].memberId = %v, want %d", index, memberID, wantID)
		}
	}
}

func TestProgressReporterImmediatelyFollowsProgressDuringRequest(t *testing.T) {
	manager := &Manager{state: positionReporterState()}
	client := &blockingPositionClient{
		requests: make(chan *wire.OpMsg, 2),
		releases: make(chan struct{}, 2),
		closed:   make(chan struct{}),
	}
	reporter := NewProgressReporter(manager, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reporter.interval = time.Hour
	reporter.timeout = time.Second
	reporter.newClient = func(string) heartbeatClient { return client }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		reporter.Run(ctx)
		close(done)
	}()
	reporter.Notify()
	waitForPositionRequest(t, client.requests)
	reporter.Notify()
	client.releases <- struct{}{}
	waitForPositionRequest(t, client.requests)
	client.releases <- struct{}{}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("progress reporter did not stop")
	}
	select {
	case <-client.closed:
	default:
		t.Fatal("progress reporter did not close its client")
	}
}

func positionReporterState() Snapshot {
	configuration := &control.ReplicaConfiguration{Version: 17}
	return Snapshot{
		SetName:       "rs0",
		MemberHost:    "dumbo.example:27017",
		MemberID:      4,
		SyncSource:    "mongo.example:27017",
		Configuration: configuration,
		Checkpoint: control.Checkpoint{
			Fetched:  control.OpTime{Seconds: 100, Increment: 9, Term: 3},
			Buffered: control.OpTime{Seconds: 100, Increment: 8, Term: 3},
			Written:  control.OpTime{Seconds: 100, Increment: 7, Term: 3},
			Durable:  control.OpTime{Seconds: 100, Increment: 6, Term: 3},
			Applied:  control.OpTime{Seconds: 100, Increment: 5, Term: 3},
		},
		Members: make(map[int]MemberStatus),
	}
}

func assertReportedOpTime(t *testing.T, position *types.Document, field string, expected control.OpTime) {
	t.Helper()
	value, _ := position.Get(field)
	document, ok := value.(*types.Document)
	if !ok {
		t.Fatalf("%s = %T, want document", field, value)
	}
	timestamp, _ := document.Get("ts")
	wantTimestamp := types.Timestamp(uint64(expected.Seconds)<<32 | uint64(expected.Increment))
	if timestamp != wantTimestamp {
		t.Fatalf("%s.ts = %v, want %v", field, timestamp, wantTimestamp)
	}
	term, _ := document.Get("t")
	if term != expected.Term {
		t.Fatalf("%s.t = %v, want %d", field, term, expected.Term)
	}
}

func waitForPositionRequest(t *testing.T, requests <-chan *wire.OpMsg) {
	t.Helper()
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for position report")
	}
}

type blockingPositionClient struct {
	requests chan *wire.OpMsg
	releases chan struct{}
	closed   chan struct{}
}

func (c *blockingPositionClient) Request(ctx context.Context, request *wire.OpMsg) (*wire.OpMsg, error) {
	select {
	case c.requests <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-c.releases:
		return wire.MustOpMsg("ok", float64(1)), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *blockingPositionClient) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}
