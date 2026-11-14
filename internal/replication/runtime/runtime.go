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
	"runtime/debug"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/initialsync"
	"github.com/dolthub/dumbodb/internal/replication/oplog"
	"github.com/dolthub/dumbodb/internal/replication/publication"
	"github.com/dolthub/dumbodb/internal/replication/recovery"
	"github.com/dolthub/dumbodb/internal/replication/special"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/replication/transport"
)

type versionedBackend interface {
	backends.Backend
	backends.VersioningBackend
	backends.InitialSyncResetter
}

type Runtime struct {
	backend               versionedBackend
	store                 *control.Store
	manager               *topology.Manager
	logger                *slog.Logger
	bumpAuth              func()
	publisher             *publication.Publisher
	recovery              *recovery.Recovery
	counterOperationKinds func(oplog.Entry) ([]string, error)
	memberOptions         []transport.MemberOptions
	// beforeApplyForTest runs before each entry applies, in tests.
	beforeApplyForTest func(oplog.Entry)
	// fetchForTest replaces the steady fetcher's run, in tests.
	fetchForTest func(context.Context, *oplog.Buffer) error
	// interrupted is set when a failure that broke no continuity left the
	// member RECOVERING; the next steady pass returns it to SECONDARY.
	interrupted bool
	// interruption numbers the continuity loss interrupted marked.
	interruption uint64
	// failures counts retries since an entry last applied, lengthening the
	// wait before the next.
	failures int
}

// maxRetryDelay caps the wait between retries of a failure that recurs.
const maxRetryDelay = time.Minute

// retryDelay is the wait before retry n, counting from 0: one second,
// doubling to maxRetryDelay.
func retryDelay(n int) time.Duration {
	if n >= 6 {
		return maxRetryDelay
	}
	return min(time.Second<<n, maxRetryDelay)
}

// markInterrupted marks continuity lost after a failure that broke none, so
// the member stops advertising itself as current until steady replication
// resumes.
func (r *Runtime) markInterrupted() {
	r.interruption = r.manager.MarkInterrupted()
	r.interrupted = true
}

// resumeAfterInterruption returns a member left RECOVERING by
// markInterrupted to SECONDARY, once an entry applies again, so a failure
// that recurs keeps it RECOVERING. A continuity loss the fetcher marked since
// keeps it RECOVERING too, for the recovery that loss starts.
func (r *Runtime) resumeAfterInterruption() {
	if !r.interrupted {
		return
	}
	r.interrupted = false
	if _, err := r.manager.ResumeSteady(r.interruption); err != nil {
		r.logger.Warn("resuming steady replication", "err", err)
	}
}

func New(
	backend backends.Backend,
	store *control.Store,
	manager *topology.Manager,
	logger *slog.Logger,
	bumpAuthGeneration func(),
	memberOptions ...transport.MemberOptions,
) (*Runtime, error) {
	if backend == nil || store == nil || manager == nil {
		return nil, errors.New("replication runtime requires backend, control store, and topology manager")
	}
	versioned, ok := backend.(versionedBackend)
	if !ok {
		return nil, errors.New("replication runtime requires a versioned initial-sync backend")
	}
	if logger == nil {
		logger = slog.Default()
	}
	publisher, err := publication.NewPublisher(versioned, store, manager)
	if err != nil {
		return nil, err
	}
	recoveryManager, err := recovery.New(versioned, store, manager, memberOptions...)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		backend: versioned, store: store, manager: manager, logger: logger,
		bumpAuth: bumpAuthGeneration, publisher: publisher, recovery: recoveryManager,
		counterOperationKinds: oplog.CounterOperationKinds,
		memberOptions:         append([]transport.MemberOptions(nil), memberOptions...),
	}, nil
}

// Run replicates until ctx is done or replication stops on a terminal
// failure. Panics on its goroutine and while fetching or applying the oplog
// stop replication instead of exiting the process.
func (r *Runtime) Run(ctx context.Context) {
	r.manager.SetRuntimePhase("starting")
	defer func() {
		if ctx.Err() != nil {
			r.manager.SetRuntimePhase("stopped")
		}
	}()
	defer func() {
		if value := recover(); value != nil {
			r.stopReplication("replication runtime", "panic", fmt.Sprintf("panic: %v", value),
				"MongoDB replication stopped by a panic", "panic", value, "stack", string(debug.Stack()))
		}
	}()
	if failure := r.store.Snapshot().InitialSyncFailure; failure != nil {
		r.manager.SetRuntimePhase("failed")
		r.logger.Error("MongoDB replication stopped by terminal initial-sync failure", "err", failure.Message)
		return
	}
	reportContext, cancelReport := context.WithCancel(ctx)
	defer cancelReport()
	reporter := topology.NewProgressReporter(r.manager, r.logger, r.memberOptions...)
	go reporter.Run(reportContext)
	for ctx.Err() == nil {
		r.manager.SetRuntimePhase("recovering")
		if err := r.recoverPublication(ctx); err != nil {
			r.retry(ctx, "recovering replication publication", err)
			continue
		}
		if _, ok := r.store.PendingRollback(); ok {
			if err := r.runRecovery(ctx); err != nil {
				r.retry(ctx, "resuming rollback recovery", err)
				continue
			}
		}
		state, err := r.waitForSource(ctx)
		if err != nil {
			return
		}
		if r.store.Snapshot().InitialSyncPhase != control.InitialSyncComplete {
			r.manager.SetRuntimePhase("initial_sync")
			if err := r.runInitialSync(ctx, state.SyncSource); err != nil {
				if r.stopOnFatal(err) {
					return
				}
				if failure, terminal := terminalInitialSyncFailure(err); terminal {
					if recordErr := r.manager.MarkInitialSyncFailed(failure); recordErr != nil {
						r.logger.Error("recording terminal initial-sync failure", "err", errors.Join(err, recordErr))
						return
					}
					r.logger.Error("MongoDB replication stopped by terminal initial-sync failure", "err", failure.Message)
					r.manager.SetRuntimePhase("failed")
					return
				}
				r.retry(ctx, "initial sync failed", err)
				continue
			}
		}
		r.manager.SetRuntimePhase("steady")
		if err := r.runSteady(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			if errors.Is(err, oplog.ErrSourceChanged) {
				continue
			}
			if r.stopOnFatal(err) {
				return
			}
			if errors.Is(err, oplog.ErrTooStale) || errors.Is(err, oplog.ErrContinuityLost) ||
				errors.Is(err, topology.ErrSourceRollbackIDChanged) {
				recoveryErr := r.runRecovery(ctx)
				if recoveryErr != nil && !errors.Is(recoveryErr, recovery.ErrInitialSyncRequired) {
					r.retry(ctx, "rollback recovery failed", recoveryErr)
					continue
				}
				r.retry(ctx, "oplog history recovered", errors.Join(err, recoveryErr))
				continue
			}
			r.markInterrupted()
			r.retry(ctx, "steady replication stopped", err)
		}
	}
}

// runRecovery runs rollback recovery and then invalidates cached privileges:
// a rollback, even a partial one, can rewind admin.system.users and
// system.roles.
func (r *Runtime) runRecovery(ctx context.Context) error {
	err := r.recovery.Run(ctx)
	if r.bumpAuth != nil {
		r.bumpAuth()
	}
	return err
}

func terminalInitialSyncFailure(err error) (control.InitialSyncFailure, bool) {
	var unsupported *initialsync.UnsupportedBSONTypeError
	if errors.As(err, &unsupported) && unsupported.Namespace != "" && unsupported.BSONType != "" {
		return control.InitialSyncFailure{
			Namespace: unsupported.Namespace,
			BSONType:  unsupported.BSONType,
			Message:   unsupported.Error(),
		}, true
	}
	var unstorable *initialsync.UnstorableKeyError
	if errors.As(err, &unstorable) {
		return control.InitialSyncFailure{Namespace: unstorable.Namespace, Message: unstorable.Error()}, true
	}
	var unsupportedCatalog *catalog.UnsupportedFeatureError
	if errors.As(err, &unsupportedCatalog) && unsupportedCatalog.Database != "" && unsupportedCatalog.Collection != "" {
		return control.InitialSyncFailure{
			Namespace: unsupportedCatalog.Database + "." + unsupportedCatalog.Collection,
			Message:   unsupportedCatalog.Error(),
		}, true
	}
	return control.InitialSyncFailure{}, false
}

func (r *Runtime) runInitialSync(ctx context.Context, source string) error {
	buffer, err := oplog.NewBuffer(oplog.BufferLimits{Entries: 50000, Bytes: 256 << 20})
	if err != nil {
		return err
	}
	stopMonitoring := r.monitorBuffer(ctx, buffer)
	defer stopMonitoring()
	fetcher, err := oplog.NewFetcher(r.manager, buffer, r.logger, r.memberOptions...)
	if err != nil {
		return err
	}
	applier, catalogApplier, specialApplier, err := r.appliers()
	if err != nil {
		return err
	}
	connector := transport.NewMemberConnector(source, r.manager.Snapshot().MemberHost, []string{"snappy", "zstd", "zlib"}, r.memberOptions...)
	defer connector.Close()
	client, err := connector.Connection(ctx)
	if err != nil {
		return err
	}
	coordinator, err := initialsync.NewCoordinator(initialsync.CoordinatorOptions{
		Source: source, Client: client, Store: r.store, Manager: r.manager,
		Fetcher: fetcher, Buffer: buffer, Resetter: r.backend, Catalog: catalogApplier,
		Applier: applier.ForInitialSync(), Publisher: r.publisher,
		Special:       initialsync.NewSpecialDatabaseHandler(specialApplier),
		LoaderLimits:  initialsync.LoaderLimits{Documents: 10000, Bytes: 64 << 20},
		CatchUpLimits: initialsync.CatchUpLimits{Entries: 1000, Bytes: 16 << 20},
	})
	if err != nil {
		return err
	}
	if r.store.Snapshot().InitialSyncPhase != control.InitialSyncNotStarted {
		if err := coordinator.Reset(ctx); err != nil {
			return err
		}
	}
	r.logger.Info("MongoDB initial sync started", "source", source)
	result, err := coordinator.Run(ctx)
	if err != nil {
		return err
	}
	r.logger.Info("MongoDB initial sync completed", "source", source, "collections", len(result.Catalog.Collections))
	return nil
}

func (r *Runtime) runSteady(ctx context.Context) error {
	buffer, err := oplog.NewBuffer(oplog.BufferLimits{Entries: 50000, Bytes: 256 << 20})
	if err != nil {
		return err
	}
	stopMonitoring := r.monitorBuffer(ctx, buffer)
	defer stopMonitoring()
	fetcher, err := oplog.NewFetcher(r.manager, buffer, r.logger, r.memberOptions...)
	if err != nil {
		return err
	}
	applier, _, _, err := r.appliers()
	if err != nil {
		return err
	}
	fetchContext, cancelFetch := context.WithCancel(ctx)
	fetchDone := make(chan error, 1)
	fetch := fetcher.Run
	if r.fetchForTest != nil {
		fetch = func(ctx context.Context) error { return r.fetchForTest(ctx, buffer) }
	}
	go func() { fetchDone <- fetch(fetchContext) }()
	fetchStopped := false
	defer func() {
		cancelFetch()
		if !fetchStopped {
			<-fetchDone
		}
	}()
	for {
		select {
		case err := <-fetchDone:
			fetchStopped = true
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		default:
		}
		state := r.manager.Snapshot().State
		if state == topology.StateRemoved {
			return oplog.ErrSourceChanged
		}
		// RECOVERING set by markInterrupted lasts until an entry applies;
		// set by the fetcher, it waits for the fetcher to report the loss.
		if (state == topology.StateRecovering && !r.interrupted) || state == topology.StateStartup2 {
			select {
			case err := <-fetchDone:
				fetchStopped = true
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		entries := buffer.Drain(1, 16<<20)
		if len(entries) == 0 {
			waitContext, cancelWait := context.WithCancel(fetchContext)
			dataReady := make(chan error, 1)
			go func() { dataReady <- buffer.WaitForData(waitContext) }()
			select {
			case err := <-fetchDone:
				fetchStopped = true
				cancelWait()
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			case err := <-dataReady:
				cancelWait()
				if err != nil {
					return err
				}
			case <-ctx.Done():
				cancelWait()
				return ctx.Err()
			}
			continue
		}
		if r.manager.Snapshot().State == topology.StateRemoved {
			return oplog.ErrSourceChanged
		}
		if err := r.applyEntry(ctx, applier, entries[0]); err != nil {
			return err
		}
		r.failures = 0
		if r.interrupted {
			// A loss the fetcher found since wins over the resume.
			select {
			case err := <-fetchDone:
				fetchStopped = true
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			default:
				r.resumeAfterInterruption()
			}
		}
	}
}

func (r *Runtime) applyEntry(ctx context.Context, applier *oplog.Applier, entry oplog.Entry) error {
	if r.beforeApplyForTest != nil {
		r.beforeApplyForTest(entry)
	}
	if entry.Operation == "n" {
		if err := applier.Apply(ctx, entry); err != nil {
			return err
		}
		if err := r.manager.AdvanceApplied(entry.OpTime); err != nil {
			return err
		}
		r.recordAppliedEntry(entry)
		return nil
	}
	databases, err := applier.AffectedDatabases(ctx, entry)
	if err != nil {
		return err
	}
	checkpoint := r.manager.Snapshot().Checkpoint
	checkpoint.Written = entry.OpTime
	checkpoint.Durable = entry.OpTime
	checkpoint.Applied = entry.OpTime
	publicationID, err := r.publisher.Begin(entry.OpTime, entry.OpTime, databases, checkpoint)
	if errors.Is(err, control.ErrPublicationComplete) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := applier.Apply(ctx, entry); err != nil {
		if recoverErr := r.abortApplyingPublication(ctx, publicationID, databases); recoverErr != nil {
			return errors.Join(err, recoverErr)
		}
		return err
	}
	if err := r.publisher.MarkReady(publicationID); err != nil {
		return err
	}
	if err := r.publisher.Complete(ctx, publicationID); err != nil {
		return err
	}
	r.recordAppliedEntry(entry)
	if len(databases) != 0 {
		r.manager.RecordPublishedCommit()
	}
	return nil
}

func (r *Runtime) recordAppliedEntry(entry oplog.Entry) {
	operationKinds, err := r.counterOperationKinds(entry)
	if err != nil {
		r.logger.Warn("counting replicated operations failed", "opTime", entry.OpTime, "err", err)
		operationKinds = nil
	}
	r.manager.RecordAppliedEntry(operationKinds)
}

func (r *Runtime) recoverPublication(ctx context.Context) error {
	pending, ok := r.store.PendingPublication()
	if !ok {
		return nil
	}
	if pending.Ready {
		return r.publisher.Complete(ctx, pending.ID)
	}
	return r.abortApplyingPublication(ctx, pending.ID, pending.Databases)
}

func (r *Runtime) abortApplyingPublication(ctx context.Context, publicationID string, databases []string) error {
	for _, database := range databases {
		if _, err := r.backend.DumboDBReset(ctx, &backends.ResetParams{
			DBName: database, Branch: "main", CommitID: "main", Hard: true,
		}); err != nil {
			return fmt.Errorf("resetting incomplete publication database %q: %w", database, err)
		}
	}
	if err := r.store.AbortPublication(publicationID); err != nil {
		return err
	}
	return r.manager.ResetFetchProgress()
}

func (r *Runtime) appliers() (*oplog.Applier, *catalog.Applier, *special.Applier, error) {
	applier, err := oplog.NewApplierWithAuthGeneration(r.backend, r.store, r.bumpAuth)
	if err != nil {
		return nil, nil, nil, err
	}
	catalogApplier, err := catalog.NewApplier(r.backend, r.store)
	if err != nil {
		return nil, nil, nil, err
	}
	specialApplier, err := special.NewApplier(r.backend, r.store, r.bumpAuth)
	if err != nil {
		return nil, nil, nil, err
	}
	return applier, catalogApplier, specialApplier, nil
}

func (r *Runtime) waitForSource(ctx context.Context) (topology.Snapshot, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state := r.manager.Snapshot()
		if state.State == topology.StateRemoved {
			r.manager.SetRuntimePhase("detached")
		} else {
			r.manager.SetRuntimePhase("waiting_for_source")
		}
		if state.State != topology.StateRemoved && state.Configuration != nil && state.SyncSource != "" {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return topology.Snapshot{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// stopOnFatal stops replication when err carries a panic raised while
// fetching or applying the oplog, or an entry with a key too long to store.
// Retrying would hit the same entry again.
func (r *Runtime) stopOnFatal(err error) bool {
	var unstorable *oplog.UnstorableKeyError
	if errors.As(err, &unstorable) {
		r.stopReplication("applying oplog entry", "key_too_long", err.Error(),
			"MongoDB replication stopped by an oplog entry with a key DumboDB cannot store",
			"opTime", unstorable.OpTime, "ns", unstorable.Namespace, "op", unstorable.Operation, "err", err)
		return true
	}
	var applyPanic *oplog.ApplyPanicError
	if errors.As(err, &applyPanic) {
		r.stopReplication("applying oplog entry", "apply_panic", err.Error(),
			"MongoDB replication stopped by a panic applying an oplog entry",
			"opTime", applyPanic.OpTime, "ns", applyPanic.Namespace, "op", applyPanic.Operation,
			"err", err, "stack", string(applyPanic.Stack))
		return true
	}
	var fetchPanic *oplog.FetchPanicError
	if errors.As(err, &fetchPanic) {
		r.stopReplication("oplog_fetch", "fetch_panic", err.Error(),
			"MongoDB replication stopped by a panic fetching the oplog",
			"source", fetchPanic.Source, "err", err, "stack", string(fetchPanic.Stack))
		return true
	}
	return false
}

func (r *Runtime) stopReplication(stage, classification, message, logMessage string, logArgs ...any) {
	r.manager.SetRuntimePhase("failed")
	// A member that applies nothing more must not report itself as a
	// current SECONDARY, which hello's secondary reads.
	if err := r.manager.MarkContinuityLost(false); err != nil {
		r.logger.Error("marking replication continuity lost", "err", err)
	}
	failure := control.ReplicationFailure{
		Stage: stage, Classification: classification, Message: message, Retryable: false,
	}
	if recordErr := r.manager.RecordFailure(failure); recordErr != nil {
		r.logger.Error("recording replication failure", "err", recordErr)
	}
	r.logger.Error(logMessage, logArgs...)
}

func (r *Runtime) retry(ctx context.Context, message string, err error) {
	if ctx.Err() != nil {
		return
	}
	r.manager.SetRuntimePhase("retrying")
	failure := control.ReplicationFailure{
		Stage: message, Classification: classifyReplicationFailure(err), Message: err.Error(), Retryable: true,
	}
	if recordErr := r.manager.RecordFailure(failure); recordErr != nil {
		r.logger.Error("recording replication failure", "err", recordErr)
	}
	r.logger.Warn(message, "err", err)
	timer := time.NewTimer(retryDelay(r.failures))
	r.failures++
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func classifyReplicationFailure(err error) string {
	switch {
	case errors.Is(err, oplog.ErrTooStale):
		return "oplog_too_stale"
	case errors.Is(err, oplog.ErrContinuityLost):
		return "oplog_continuity_lost"
	case errors.Is(err, topology.ErrSourceRollbackIDChanged):
		return "source_rollback"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "transient"
	}
}

func (r *Runtime) monitorBuffer(ctx context.Context, buffer *oplog.Buffer) func() {
	monitorContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	update := func() {
		stats := buffer.Stats()
		limits := buffer.Limits()
		r.manager.ObserveBuffer(stats.Entries, stats.Bytes, limits.Entries, limits.Bytes)
	}
	update()
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorContext.Done():
				return
			case <-ticker.C:
				update()
			}
		}
	}()
	return func() {
		cancel()
		<-done
		limits := buffer.Limits()
		r.manager.ObserveBuffer(0, 0, limits.Entries, limits.Bytes)
	}
}
