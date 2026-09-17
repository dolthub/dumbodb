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
	"io"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/oplog"
	"github.com/dolthub/dumbodb/internal/replication/topology"
)

// ErrSpecialDatabaseHandlerRequired prevents admin or config from being silently omitted.
var ErrSpecialDatabaseHandlerRequired = errors.New("initial sync requires an admin/config translation handler")

type initialSyncFetcher interface {
	FetchFrom(context.Context, control.OpTime) error
}

// CompletionPublisher durably publishes reconciled replica state before progress is reported.
type CompletionPublisher interface {
	PublishInitialSync(context.Context, InitialSyncPublication) error
}

// InitialSyncPublication identifies the candidate state and source interval to publish.
type InitialSyncPublication struct {
	Attempt    control.InitialSyncAttempt
	Catalog    CatalogMaterialization
	Checkpoint control.Checkpoint
}

// SpecialDatabaseHandler translates admin and config without creating ordinary databases.
type SpecialDatabaseHandler func(context.Context, requestClient, []Database, control.OpTime) error

// CoordinatorOptions supplies one logical initial-sync attempt.
type CoordinatorOptions struct {
	Source        string
	Client        requestClient
	Store         *control.Store
	Manager       *topology.Manager
	Fetcher       initialSyncFetcher
	Buffer        *oplog.Buffer
	Resetter      backends.InitialSyncResetter
	Catalog       *catalog.Applier
	Applier       oplogEntryApplier
	Publisher     CompletionPublisher
	Special       SpecialDatabaseHandler
	LoaderLimits  LoaderLimits
	CatchUpLimits CatchUpLimits
}

// Coordinator runs source discovery, concurrent fetch, clone, catch-up, and publication.
type Coordinator struct {
	options CoordinatorOptions
}

// InitialSyncResult reports the boundaries and completed clone work.
type InitialSyncResult struct {
	Attempt    control.InitialSyncAttempt
	Catalog    CatalogMaterialization
	CatchUp    CatchUpResult
	Checkpoint control.Checkpoint
}

func NewCoordinator(options CoordinatorOptions) (*Coordinator, error) {
	if options.Source == "" || options.Client == nil || options.Store == nil || options.Manager == nil ||
		options.Fetcher == nil || options.Buffer == nil || options.Resetter == nil || options.Catalog == nil ||
		options.Applier == nil || options.Publisher == nil {
		return nil, errors.New("initial sync coordinator requires source, client, store, manager, fetcher, buffer, resetter, catalog, applier, and publisher")
	}
	if options.LoaderLimits.Documents <= 0 || options.LoaderLimits.Bytes <= 0 ||
		options.CatchUpLimits.Entries <= 0 || options.CatchUpLimits.Bytes <= 0 {
		return nil, errors.New("initial sync coordinator requires positive loader and catch-up limits")
	}
	return &Coordinator{options: options}, nil
}

// Reset discards replica and control state from an incomplete or obsolete attempt.
func (c *Coordinator) Reset(ctx context.Context) error {
	state := c.options.Store.Snapshot()
	if err := c.options.Resetter.ResetInitialSyncData(ctx); err != nil {
		return fmt.Errorf("resetting initial sync data: %w", err)
	}
	c.options.Buffer.Reset()
	attemptID := ""
	if state.InitialSyncAttempt != nil {
		attemptID = state.InitialSyncAttempt.ID
	}
	return c.options.Manager.ResetInitialSync(attemptID)
}

// Run executes one disposable initial-sync attempt.
func (c *Coordinator) Run(ctx context.Context) (result InitialSyncResult, err error) {
	if phase := c.options.Store.Snapshot().InitialSyncPhase; phase != control.InitialSyncNotStarted {
		return InitialSyncResult{}, fmt.Errorf("cannot start initial sync in phase %q", phase)
	}
	attempt, err := DiscoverBoundaries(ctx, c.options.Client, c.options.Source)
	if err != nil {
		return InitialSyncResult{}, err
	}
	if err := c.options.Resetter.ResetInitialSyncData(ctx); err != nil {
		return InitialSyncResult{}, fmt.Errorf("clearing data for initial sync: %w", err)
	}
	c.options.Buffer.Reset()
	if err := c.options.Store.BeginInitialSync(attempt); err != nil {
		return InitialSyncResult{}, err
	}
	result.Attempt = attempt

	fetchContext, cancelFetch := context.WithCancel(ctx)
	fetchDone := make(chan error, 1)
	go func() {
		fetchErr := c.options.Fetcher.FetchFrom(fetchContext, attempt.BeginFetch)
		fetchDone <- fetchErr
		cancelFetch()
	}()
	defer func() {
		cancelFetch()
		fetchErr := <-fetchDone
		if ctx.Err() != nil {
			err = ctx.Err()
			return
		}
		if err != nil && errors.Is(err, context.Canceled) && !errors.Is(fetchErr, context.Canceled) && !errors.Is(fetchErr, context.DeadlineExceeded) {
			err = fmt.Errorf("initial sync oplog fetch ended: %w", fetchErr)
			return
		}
		if err == nil && !errors.Is(fetchErr, context.Canceled) && !errors.Is(fetchErr, context.DeadlineExceeded) && !errors.Is(fetchErr, io.EOF) {
			err = fmt.Errorf("stopping initial sync oplog fetch: %w", fetchErr)
		}
	}()

	databases, err := DiscoverCatalog(fetchContext, c.options.Client)
	if err != nil {
		return result, err
	}
	result.Catalog, err = MaterializeOrdinaryCatalog(fetchContext, c.options.Client, c.options.Catalog, databases,
		attempt.BeginApply, c.options.LoaderLimits, func(progress MaterializationProgress) {
			c.options.Manager.ObserveInitialSyncProgress(
				progress.CollectionsTotal, progress.CollectionsCompleted, progress.Documents, progress.CurrentNamespace,
			)
		})
	if err != nil {
		return result, err
	}
	if len(result.Catalog.SpecialDatabases) != 0 {
		if c.options.Special == nil {
			return result, ErrSpecialDatabaseHandlerRequired
		}
		if err := c.options.Special(fetchContext, c.options.Client, result.Catalog.SpecialDatabases, attempt.BeginApply); err != nil {
			return result, fmt.Errorf("translating special databases: %w", err)
		}
	}
	attempt.Stop, err = ReadStopPosition(fetchContext, c.options.Client, attempt.BeginApply)
	if err != nil {
		return result, err
	}
	result.Attempt.Stop = attempt.Stop
	if err := c.options.Store.SetInitialSyncStop(attempt.ID, attempt.Stop); err != nil {
		return result, err
	}
	result.CatchUp, err = ApplyBufferedThrough(fetchContext, c.options.Buffer, c.options.Applier,
		attempt.BeginApply, attempt.Stop, c.options.CatchUpLimits)
	if err != nil {
		return result, err
	}
	if err := VerifyRollbackID(fetchContext, c.options.Client, attempt.SourceRBID); err != nil {
		return result, err
	}
	checkpoint := c.options.Manager.Snapshot().Checkpoint
	if checkpoint.Fetched.Compare(attempt.Stop) < 0 || checkpoint.Buffered.Compare(attempt.Stop) < 0 {
		return result, fmt.Errorf("oplog fetch checkpoint %+v does not reach initial sync stop %v", checkpoint, attempt.Stop)
	}
	checkpoint.Written = attempt.Stop
	checkpoint.Durable = attempt.Stop
	checkpoint.Applied = attempt.Stop
	publication := InitialSyncPublication{Attempt: attempt, Catalog: result.Catalog, Checkpoint: checkpoint}
	if err := c.options.Publisher.PublishInitialSync(fetchContext, publication); err != nil {
		return result, fmt.Errorf("publishing initial sync: %w", err)
	}
	if err := c.options.Manager.CompleteInitialSync(attempt.ID, checkpoint, attempt.SourceRBID); err != nil {
		return result, err
	}
	result.Checkpoint = checkpoint
	return result, nil
}
