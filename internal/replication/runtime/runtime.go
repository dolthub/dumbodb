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

// Package runtime drives initial sync and ongoing oplog application.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
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
	backend   versionedBackend
	store     *control.Store
	manager   *topology.Manager
	logger    *slog.Logger
	bumpAuth  func()
	publisher *publication.Publisher
	recovery  *recovery.Recovery
}

func New(
	backend backends.Backend,
	store *control.Store,
	manager *topology.Manager,
	logger *slog.Logger,
	bumpAuthGeneration func(),
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
	recoveryManager, err := recovery.New(versioned, store, manager)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		backend: versioned, store: store, manager: manager, logger: logger,
		bumpAuth: bumpAuthGeneration, publisher: publisher, recovery: recoveryManager,
	}, nil
}

func (r *Runtime) Run(ctx context.Context) {
	r.manager.SetRuntimePhase("starting")
	defer func() {
		if ctx.Err() != nil {
			r.manager.SetRuntimePhase("stopped")
		}
	}()
	if failure := r.store.Snapshot().InitialSyncFailure; failure != nil {
		r.manager.SetRuntimePhase("failed")
		r.logger.Error("MongoDB replication stopped by terminal initial-sync failure", "err", failure.Message)
		return
	}
	reportContext, cancelReport := context.WithCancel(ctx)
	defer cancelReport()
	reporter := topology.NewProgressReporter(r.manager, r.logger)
	go reporter.Run(reportContext)
	for ctx.Err() == nil {
		r.manager.SetRuntimePhase("recovering")
		if err := r.recoverPublication(ctx); err != nil {
			r.retry(ctx, "recovering replication publication", err)
			continue
		}
		if _, ok := r.store.PendingRollback(); ok {
			if err := r.recovery.Run(ctx); err != nil {
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
			if errors.Is(err, oplog.ErrTooStale) || errors.Is(err, oplog.ErrContinuityLost) ||
				errors.Is(err, topology.ErrSourceRollbackIDChanged) {
				recoveryErr := r.recovery.Run(ctx)
				if recoveryErr != nil && !errors.Is(recoveryErr, recovery.ErrInitialSyncRequired) {
					r.retry(ctx, "rollback recovery failed", recoveryErr)
					continue
				}
				r.retry(ctx, "oplog history recovered", errors.Join(err, recoveryErr))
				continue
			}
			_ = r.manager.MarkContinuityLost(false)
			r.retry(ctx, "steady replication stopped", err)
		}
	}
}

func terminalInitialSyncFailure(err error) (control.InitialSyncFailure, bool) {
	var unsupported *initialsync.UnsupportedBSONTypeError
	if !errors.As(err, &unsupported) || unsupported.Namespace == "" || unsupported.BSONType == "" {
		return control.InitialSyncFailure{}, false
	}
	return control.InitialSyncFailure{
		Namespace: unsupported.Namespace,
		BSONType:  unsupported.BSONType,
		Message:   unsupported.Error(),
	}, true
}

func (r *Runtime) runInitialSync(ctx context.Context, source string) error {
	buffer, err := oplog.NewBuffer(oplog.BufferLimits{Entries: 50000, Bytes: 256 << 20})
	if err != nil {
		return err
	}
	stopMonitoring := r.monitorBuffer(ctx, buffer)
	defer stopMonitoring()
	fetcher, err := oplog.NewFetcher(r.manager, buffer, r.logger)
	if err != nil {
		return err
	}
	applier, catalogApplier, specialApplier, err := r.appliers()
	if err != nil {
		return err
	}
	connector := transport.NewMemberConnector(source, r.manager.Snapshot().MemberHost, []string{"snappy", "zstd", "zlib"})
	defer connector.Close()
	client, err := connector.Connection(ctx)
	if err != nil {
		return err
	}
	coordinator, err := initialsync.NewCoordinator(initialsync.CoordinatorOptions{
		Source: source, Client: client, Store: r.store, Manager: r.manager,
		Fetcher: fetcher, Buffer: buffer, Resetter: r.backend, Catalog: catalogApplier,
		Applier: applier, Publisher: r.publisher,
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
	fetcher, err := oplog.NewFetcher(r.manager, buffer, r.logger)
	if err != nil {
		return err
	}
	applier, _, _, err := r.appliers()
	if err != nil {
		return err
	}
	fetchContext, cancelFetch := context.WithCancel(ctx)
	fetchDone := make(chan error, 1)
	go func() { fetchDone <- fetcher.Run(fetchContext) }()
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
		if state == topology.StateRecovering || state == topology.StateStartup2 {
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
		if err := r.applyEntry(ctx, applier, entries[0]); err != nil {
			return err
		}
	}
}

func (r *Runtime) applyEntry(ctx context.Context, applier *oplog.Applier, entry oplog.Entry) error {
	databases, err := r.publicationDatabases(ctx, entry)
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
	r.manager.RecordAppliedOperation(entry.Operation)
	return nil
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

func (r *Runtime) publicationDatabases(ctx context.Context, entry oplog.Entry) ([]string, error) {
	result, err := r.backend.ListDatabases(ctx, nil)
	if err != nil {
		return nil, err
	}
	databases := make([]string, 0, len(result.Databases)+1)
	for _, database := range result.Databases {
		if database.Name != "config" && database.Name != "local" {
			databases = append(databases, database.Name)
		}
	}
	if database, _, ok := strings.Cut(entry.Namespace, "."); ok && database != "" && database != "config" && database != "local" {
		databases = append(databases, database)
	}
	return databases, nil
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
	timer := time.NewTimer(time.Second)
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
