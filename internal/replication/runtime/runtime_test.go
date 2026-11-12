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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/initialsync"
	"github.com/dolthub/dumbodb/internal/replication/oplog"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestTerminalInitialSyncFailureClassifiesUnsupportedData(t *testing.T) {
	unsupported := &initialsync.UnsupportedBSONTypeError{
		Namespace: "archive.items",
		BSONType:  "JavaScript",
	}
	failure, terminal := terminalInitialSyncFailure(fmt.Errorf("initial sync: %w", unsupported))
	if !terminal || failure.Namespace != unsupported.Namespace || failure.BSONType != unsupported.BSONType || failure.Message != unsupported.Error() {
		t.Fatalf("terminal failure = %+v, %v", failure, terminal)
	}
	unsupportedCatalog := &catalog.UnsupportedFeatureError{
		Database: "archive", Collection: "active_items", Option: "viewOn", Reason: "view replication is unsupported",
	}
	failure, terminal = terminalInitialSyncFailure(fmt.Errorf("initial sync: %w", unsupportedCatalog))
	if !terminal || failure.Namespace != "archive.active_items" || failure.BSONType != "" || failure.Message != unsupportedCatalog.Error() {
		t.Fatalf("terminal catalog failure = %+v, %v", failure, terminal)
	}
	if failure, terminal := terminalInitialSyncFailure(errors.New("connection reset")); terminal || failure != (control.InitialSyncFailure{}) {
		t.Fatalf("transient failure = %+v, %v", failure, terminal)
	}
}

func TestApplyNoopAdvancesCheckpointWithoutCommit(t *testing.T) {
	ctx := context.Background()
	replicationRuntime, backend, store := newRecoveryRuntime(t)
	recoveryCollection(t, ctx, backend)
	commitRecoveryDatabase(t, ctx, backend, "base")
	versioned := backend.(backends.VersioningBackend)
	before, err := versioned.DumboDBLog(ctx, &backends.LogParams{DBName: "recovery", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	document := must.NotFail(types.NewDocument(
		"ts", types.Timestamp(uint64(100)<<32|1),
		"t", int64(2),
		"op", "n",
		"ns", "",
		"o", must.NotFail(types.NewDocument("msg", "periodic noop")),
	))
	entry, err := oplog.ParseEntry(document)
	if err != nil {
		t.Fatal(err)
	}
	replicationRuntime.counterOperationKinds = func(oplog.Entry) ([]string, error) {
		return nil, errors.New("injected counter failure")
	}
	if err := replicationRuntime.manager.AdvanceFetched(entry.OpTime, entry.OpTime); err != nil {
		t.Fatal(err)
	}
	applier, _, _, err := replicationRuntime.appliers()
	if err != nil {
		t.Fatal(err)
	}
	if err := replicationRuntime.applyEntry(ctx, applier, entry); err != nil {
		t.Fatal(err)
	}
	after, err := versioned.DumboDBLog(ctx, &backends.LogParams{DBName: "recovery", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Commits) != len(before.Commits) {
		t.Fatalf("noop changed commit count from %d to %d", len(before.Commits), len(after.Commits))
	}
	checkpoint := store.Snapshot().Checkpoint
	if checkpoint.Written != entry.OpTime || checkpoint.Durable != entry.OpTime || checkpoint.Applied != entry.OpTime {
		t.Fatalf("noop checkpoint = %+v, want applied %+v", checkpoint, entry.OpTime)
	}
	if _, ok := store.CommitFor(entry.OpTime); ok {
		t.Fatal("noop created commit provenance")
	}
	runtimeStatus := replicationRuntime.manager.Snapshot().Runtime
	if runtimeStatus.AppliedOperations != 1 || runtimeStatus.PublishedCommits != 0 {
		t.Fatalf("noop runtime counters = %+v", runtimeStatus)
	}
}

func TestApplyEntryCommitsOnlyAffectedDatabase(t *testing.T) {
	ctx := context.Background()
	replicationRuntime, backend, store := newRecoveryRuntime(t)
	catalogApplier, err := catalog.NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	sourceUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	if _, err := catalogApplier.Create(ctx, "orders", backends.CreateCollectionParams{Name: "items"}, sourceUUID.String(), control.OpTime{Seconds: 99, Increment: 1, Term: 2}); err != nil {
		t.Fatal(err)
	}
	archive, err := backend.Database("archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "events"}); err != nil {
		t.Fatal(err)
	}
	versioned := backend.(backends.VersioningBackend)
	for _, database := range []string{"archive", "orders"} {
		if _, err := versioned.DumboDBCommit(ctx, &backends.CommitParams{
			DBName: database, Branch: "main", Message: "base", Author: "Test <test@example.com>",
		}); err != nil {
			t.Fatal(err)
		}
	}
	beforeArchive, err := versioned.DumboDBLog(ctx, &backends.LogParams{DBName: "archive", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	beforeOrders, err := versioned.DumboDBLog(ctx, &backends.LogParams{DBName: "orders", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	document := must.NotFail(types.NewDocument(
		"ts", types.Timestamp(uint64(100)<<32|1), "t", int64(2),
		"op", "i", "ns", "orders.items",
		"ui", types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]},
		"o", must.NotFail(types.NewDocument("_id", int32(1))),
	))
	entry, err := oplog.ParseEntry(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := replicationRuntime.manager.AdvanceFetched(entry.OpTime, entry.OpTime); err != nil {
		t.Fatal(err)
	}
	applier, _, _, err := replicationRuntime.appliers()
	if err != nil {
		t.Fatal(err)
	}
	if err := replicationRuntime.applyEntry(ctx, applier, entry); err != nil {
		t.Fatal(err)
	}
	afterArchive, err := versioned.DumboDBLog(ctx, &backends.LogParams{DBName: "archive", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	afterOrders, err := versioned.DumboDBLog(ctx, &backends.LogParams{DBName: "orders", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterArchive.Commits) != len(beforeArchive.Commits) {
		t.Fatalf("unaffected archive history grew from %d to %d commits", len(beforeArchive.Commits), len(afterArchive.Commits))
	}
	if len(afterOrders.Commits) != len(beforeOrders.Commits)+1 {
		t.Fatalf("orders history grew from %d to %d commits, want one", len(beforeOrders.Commits), len(afterOrders.Commits))
	}
	interval, ok := store.CommitFor(entry.OpTime)
	if !ok || len(interval.Commits) != 1 || interval.Commits[0].Database != "orders" {
		t.Fatalf("published interval = %+v, %v", interval, ok)
	}
	if published := replicationRuntime.manager.Snapshot().Runtime.PublishedCommits; published != 1 {
		t.Fatalf("published commits = %d, want 1", published)
	}
}

func TestApplyClusterKeyPublishesAdminCommit(t *testing.T) {
	ctx := context.Background()
	replicationRuntime, _, store := newRecoveryRuntime(t)
	document := must.NotFail(types.NewDocument(
		"ts", types.Timestamp(uint64(100)<<32|1), "t", int64(2),
		"op", "i", "ns", "admin.system.keys",
		"o", must.NotFail(types.NewDocument(
			"_id", int64(7), "purpose", "HMAC",
			"key", types.Binary{Subtype: types.BinaryGeneric, B: make([]byte, 20)},
			"expiresAt", types.Timestamp(uint64(200)<<32|1),
		)),
	))
	entry, err := oplog.ParseEntry(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := replicationRuntime.manager.AdvanceFetched(entry.OpTime, entry.OpTime); err != nil {
		t.Fatal(err)
	}
	applier, _, _, err := replicationRuntime.appliers()
	if err != nil {
		t.Fatal(err)
	}
	if err := replicationRuntime.applyEntry(ctx, applier, entry); err != nil {
		t.Fatal(err)
	}
	interval, ok := store.CommitFor(entry.OpTime)
	if !ok || len(interval.Commits) != 1 || interval.Commits[0].Database != "admin" {
		t.Fatalf("cluster key interval = %+v, %v", interval, ok)
	}
	status := replicationRuntime.manager.Snapshot().Runtime
	if status.AppliedOperations != 1 || status.PublishedCommits != 1 {
		t.Fatalf("cluster key runtime counters = %+v", status)
	}
}

func TestRecoverPublicationDiscardsIncompleteApply(t *testing.T) {
	ctx := context.Background()
	runtime, backend, store := newRecoveryRuntime(t)
	collection := recoveryCollection(t, ctx, backend)
	insertRecoveryDocument(t, ctx, collection, 1)
	commitRecoveryDatabase(t, ctx, backend, "base")
	insertRecoveryDocument(t, ctx, collection, 2)

	position := control.OpTime{Seconds: 100, Increment: 2, Term: 1}
	checkpoint := control.Checkpoint{
		Fetched: position, Buffered: position, Written: position, Durable: position, Applied: position,
	}
	publicationID, err := runtime.publisher.Begin(position, position, []string{"recovery"}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	mapping := control.CollectionMapping{
		SourceUUID: "created-during-apply", Database: "recovery", Collection: "events", LocalUUID: "local-created",
		CreateOpTime: position, LastUpdateOpTime: position,
	}
	if err := store.PutCollectionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	if err := runtime.recoverPublication(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.PendingPublication(); ok {
		t.Fatal("incomplete publication remained pending")
	}
	if count := recoveryCount(t, ctx, collection); count != 1 {
		t.Fatalf("document count after recovering %s = %d, want 1", publicationID, count)
	}
	if _, ok := store.CollectionMapping(mapping.SourceUUID); ok {
		t.Fatal("control mapping from incomplete publication survived recovery")
	}
}

func TestRecoverPublicationCompletesReadyApply(t *testing.T) {
	ctx := context.Background()
	runtime, backend, store := newRecoveryRuntime(t)
	collection := recoveryCollection(t, ctx, backend)
	insertRecoveryDocument(t, ctx, collection, 1)
	commitRecoveryDatabase(t, ctx, backend, "base")
	insertRecoveryDocument(t, ctx, collection, 2)

	position := control.OpTime{Seconds: 100, Increment: 2, Term: 1}
	checkpoint := control.Checkpoint{
		Fetched: position, Buffered: position, Written: position, Durable: position, Applied: position,
	}
	publicationID, err := runtime.publisher.Begin(position, position, []string{"recovery"}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.publisher.MarkReady(publicationID); err != nil {
		t.Fatal(err)
	}
	if err := runtime.recoverPublication(ctx); err != nil {
		t.Fatal(err)
	}
	if count := recoveryCount(t, ctx, collection); count != 2 {
		t.Fatalf("document count after ready recovery = %d, want 2", count)
	}
	if interval, ok := store.CommitFor(position); !ok || interval.CommitID != publicationID {
		t.Fatalf("recovered commit interval = %+v, %v", interval, ok)
	}
}

func newRecoveryRuntime(t *testing.T) (*Runtime, backends.Backend, *control.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	backend, err := dolt.NewBackend(t.TempDir(), logger, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	store, err := control.Open(backend, control.Configuration{SetName: "rs0", MemberHost: "dumbo.example:27017"})
	if err != nil {
		t.Fatal(err)
	}
	manager := topology.New(store)
	runtime, err := New(backend, store, manager, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, backend, store
}

func recoveryCollection(t *testing.T, ctx context.Context, backend backends.Backend) backends.Collection {
	t.Helper()
	database, err := backend.Database("recovery")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "events"}); err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("events")
	if err != nil {
		t.Fatal(err)
	}
	return collection
}

func insertRecoveryDocument(t *testing.T, ctx context.Context, collection backends.Collection, id int32) {
	t.Helper()
	document := must.NotFail(types.NewDocument("_id", id))
	if _, err := collection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{document}}); err != nil {
		t.Fatal(err)
	}
}

func commitRecoveryDatabase(t *testing.T, ctx context.Context, backend backends.Backend, message string) {
	t.Helper()
	versioned := backend.(backends.VersioningBackend)
	if _, err := versioned.DumboDBCommit(ctx, &backends.CommitParams{
		DBName: "recovery", Branch: "main", Message: message, Author: "Test <test@example.com>",
	}); err != nil {
		t.Fatal(err)
	}
}

func recoveryCount(t *testing.T, ctx context.Context, collection backends.Collection) int64 {
	t.Helper()
	result, err := collection.Count(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.Count
}

// Rollback recovery can rewind admin.system.users and system.roles, so
// cached privileges must be invalidated afterwards; before the fix only the
// oplog and special-namespace appliers bumped the auth generation.
func TestRollbackRecoveryInvalidatesCachedPrivileges(t *testing.T) {
	runtime, _, _ := newRecoveryRuntime(t)
	bumps := 0
	runtime.bumpAuth = func() { bumps++ }

	_ = runtime.runRecovery(context.Background())
	if bumps != 1 {
		t.Fatalf("auth generation bumps after recovery = %d, want 1", bumps)
	}
}

// panicOnQueryBackend panics on every Query of collection panicCollection.
type panicOnQueryBackend struct {
	backends.Backend
	panicCollection string
}

func (b *panicOnQueryBackend) Database(name string) (backends.Database, error) {
	database, err := b.Backend.Database(name)
	if err != nil {
		return nil, err
	}
	return &panicOnQueryDatabase{Database: database, panicCollection: b.panicCollection}, nil
}

type panicOnQueryDatabase struct {
	backends.Database
	panicCollection string
}

func (d *panicOnQueryDatabase) Collection(name string) (backends.Collection, error) {
	collection, err := d.Database.Collection(name)
	if err != nil || name != d.panicCollection {
		return collection, err
	}
	return &panicOnQueryCollection{Collection: collection}, nil
}

type panicOnQueryCollection struct {
	backends.Collection
}

func (c *panicOnQueryCollection) Query(context.Context, *backends.QueryParams) (*backends.QueryResult, error) {
	panic("injected query panic")
}

// panicOnResetBackend panics on every DumboDBReset.
type panicOnResetBackend struct {
	versionedBackend
}

func (b *panicOnResetBackend) DumboDBReset(context.Context, *backends.ResetParams) (*backends.ResetResult, error) {
	panic("injected reset panic")
}

func lastReplicationFailure(t *testing.T, store *control.Store) control.ReplicationFailure {
	t.Helper()
	history := store.Snapshot().FailureHistory
	if len(history) == 0 {
		t.Fatal("no replication failure recorded")
	}
	return history[len(history)-1]
}

func TestApplyEntryPanicDiscardsPublicationAndStopsReplication(t *testing.T) {
	ctx := context.Background()
	replicationRuntime, backend, store := newRecoveryRuntime(t)
	catalogApplier, err := catalog.NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	sourceUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	if _, err := catalogApplier.Create(ctx, "recovery", backends.CreateCollectionParams{Name: "events"}, sourceUUID.String(), control.OpTime{Seconds: 99, Increment: 1, Term: 2}); err != nil {
		t.Fatal(err)
	}
	database := must.NotFail(backend.Database("recovery"))
	collection := must.NotFail(database.Collection("events"))
	insertRecoveryDocument(t, ctx, collection, 1)
	commitRecoveryDatabase(t, ctx, backend, "base")

	applier, err := oplog.NewApplier(&panicOnQueryBackend{Backend: backend, panicCollection: "events"}, store)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := oplog.ParseEntry(must.NotFail(types.NewDocument(
		"ts", types.Timestamp(uint64(100)<<32|1), "t", int64(2), "op", "d", "ns", "recovery.events",
		"ui", types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]},
		"o", must.NotFail(types.NewDocument("_id", int32(1))),
	)))
	if err != nil {
		t.Fatal(err)
	}
	if err := replicationRuntime.manager.AdvanceFetched(entry.OpTime, entry.OpTime); err != nil {
		t.Fatal(err)
	}
	applyErr := replicationRuntime.applyEntry(ctx, applier, entry)
	var panicErr *oplog.ApplyPanicError
	if !errors.As(applyErr, &panicErr) || panicErr.OpTime != entry.OpTime {
		t.Fatalf("applyEntry error = %v, want *oplog.ApplyPanicError at %v", applyErr, entry.OpTime)
	}
	if _, ok := store.PendingPublication(); ok {
		t.Fatal("publication of the panicking entry remained pending")
	}
	if count := recoveryCount(t, ctx, collection); count != 1 {
		t.Fatalf("document count after panic = %d, want 1", count)
	}
	if !replicationRuntime.stopOnPanic(applyErr) {
		t.Fatal("stopOnPanic did not stop on an apply panic")
	}
	if phase := replicationRuntime.manager.Snapshot().Runtime.Phase; phase != "failed" {
		t.Fatalf("runtime phase = %q, want failed", phase)
	}
	if failure := lastReplicationFailure(t, store); failure.Retryable || failure.Classification != "apply_panic" {
		t.Fatalf("recorded failure = %+v", failure)
	}
	if replicationRuntime.stopOnPanic(errors.New("connection reset")) {
		t.Fatal("stopOnPanic stopped on an ordinary error")
	}
}

func TestRunRecoversPanicAndStops(t *testing.T) {
	replicationRuntime, backend, store := newRecoveryRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	recoveryCollection(t, ctx, backend)
	position := control.OpTime{Seconds: 100, Increment: 2, Term: 1}
	checkpoint := control.Checkpoint{
		Fetched: position, Buffered: position, Written: position, Durable: position, Applied: position,
	}
	if _, err := replicationRuntime.publisher.Begin(position, position, []string{"recovery"}, checkpoint); err != nil {
		t.Fatal(err)
	}
	replicationRuntime.backend = &panicOnResetBackend{versionedBackend: replicationRuntime.backend}

	done := make(chan struct{})
	go func() {
		defer close(done)
		replicationRuntime.Run(ctx)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Run did not return after a panic")
	}
	if phase := replicationRuntime.manager.Snapshot().Runtime.Phase; phase != "failed" {
		t.Fatalf("runtime phase = %q, want failed", phase)
	}
	if failure := lastReplicationFailure(t, store); failure.Retryable || failure.Classification != "panic" {
		t.Fatalf("recorded failure = %+v", failure)
	}
}

func TestStopOnPanicStopsOnFetchPanic(t *testing.T) {
	replicationRuntime, _, store := newRecoveryRuntime(t)
	err := fmt.Errorf("initial sync oplog fetch ended: %w", &oplog.FetchPanicError{Source: "mongo-a:27017", Value: "boom"})
	if !replicationRuntime.stopOnPanic(err) {
		t.Fatal("stopOnPanic did not stop on a fetch panic")
	}
	if phase := replicationRuntime.manager.Snapshot().Runtime.Phase; phase != "failed" {
		t.Fatalf("runtime phase = %q, want failed", phase)
	}
	if failure := lastReplicationFailure(t, store); failure.Retryable || failure.Classification != "fetch_panic" {
		t.Fatalf("recorded failure = %+v", failure)
	}
}

func TestRetryDelayBacksOff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for n, w := range want {
		if got := retryDelay(n); got != w {
			t.Errorf("retryDelay(%d) = %v, want %v", n, got, w)
		}
	}
	if got := retryDelay(1000); got != time.Minute {
		t.Errorf("retryDelay(1000) = %v, want %v", got, time.Minute)
	}
}

// TestFetcherLossWinsOverResume: when the fetcher reports a continuity loss
// while the entry that would resume the member applies, the member stays
// RECOVERING and steady replication returns the loss.
func TestFetcherLossWinsOverResume(t *testing.T) {
	ctx := context.Background()
	replicationRuntime, _, store := newRecoveryRuntime(t)
	if err := store.SetInitialSyncPhase(control.InitialSyncComplete); err != nil {
		t.Fatal(err)
	}
	replicationRuntime.markInterrupted()
	entry, err := oplog.ParseEntry(must.NotFail(types.NewDocument(
		"ts", types.Timestamp(uint64(100)<<32|1),
		"t", int64(2),
		"op", "n",
		"ns", "",
		"o", must.NotFail(types.NewDocument("msg", "periodic noop")),
	)))
	if err != nil {
		t.Fatal(err)
	}
	fetcherReturned := make(chan struct{})
	replicationRuntime.fetchForTest = func(_ context.Context, buffer *oplog.Buffer) error {
		if err := replicationRuntime.manager.AdvanceFetched(entry.OpTime, entry.OpTime); err != nil {
			return err
		}
		if err := buffer.TryAppend(entry); err != nil {
			return err
		}
		// Report the loss once the entry is taken to apply.
		for buffer.Stats().Entries > 0 {
			time.Sleep(time.Millisecond)
		}
		defer close(fetcherReturned)
		return oplog.ErrContinuityLost
	}
	replicationRuntime.beforeApplyForTest = func(oplog.Entry) {
		<-fetcherReturned
		// Let the fetcher's goroutine deliver its result.
		time.Sleep(50 * time.Millisecond)
	}
	if err := replicationRuntime.runSteady(ctx); !errors.Is(err, oplog.ErrContinuityLost) {
		t.Fatalf("runSteady = %v, want the continuity loss", err)
	}
	if state := replicationRuntime.manager.Snapshot().State; state != topology.StateRecovering {
		t.Fatalf("member state = %v, want RECOVERING", state)
	}
}

// TestFetcherLossMarkedBeforeResumeKeepsRecovering: a continuity loss the
// fetcher marks before it returns keeps the member RECOVERING through the
// resume that races it.
func TestFetcherLossMarkedBeforeResumeKeepsRecovering(t *testing.T) {
	replicationRuntime, _, store := newRecoveryRuntime(t)
	if err := store.SetInitialSyncPhase(control.InitialSyncComplete); err != nil {
		t.Fatal(err)
	}
	replicationRuntime.markInterrupted()
	if err := replicationRuntime.manager.MarkContinuityLost(false); err != nil {
		t.Fatal(err)
	}
	replicationRuntime.resumeAfterInterruption()
	if state := replicationRuntime.manager.Snapshot().State; state != topology.StateRecovering {
		t.Fatalf("member state = %v, want RECOVERING", state)
	}
}

// TestInterruptionResumesWhenAnEntryApplies: a steady failure that broke no
// continuity leaves the member RECOVERING until an entry applies again.
func TestInterruptionResumesWhenAnEntryApplies(t *testing.T) {
	replicationRuntime, _, store := newRecoveryRuntime(t)
	if err := store.SetInitialSyncPhase(control.InitialSyncComplete); err != nil {
		t.Fatal(err)
	}
	replicationRuntime.markInterrupted()
	if state := replicationRuntime.manager.Snapshot().State; state != topology.StateRecovering {
		t.Fatalf("member state = %v, want RECOVERING", state)
	}
	replicationRuntime.resumeAfterInterruption()
	if state := replicationRuntime.manager.Snapshot().State; state != topology.StateSecondary {
		t.Fatalf("member state after resuming = %v, want SECONDARY", state)
	}
}
