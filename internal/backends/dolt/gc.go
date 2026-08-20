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

package dolt

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
)

// backgroundGCRootsProvider is the GCRootsProvider used by
// RunUnderGCSafepointKeeper to bracket background mutators (capped
// collection cleanup today; future GC-triggered tasks tomorrow). It
// holds no per-session state and reports no extra roots: every chunk
// the background tick writes goes through updateBranchWS, which fsyncs
// the on-disk working_set ref before the chunk is otherwise reachable.
// GC's standard disk-ref walk covers all of it.
//
// The bracket exists not for root accounting but to gate GC's
// pre-finalize safepoint on the tick's completion, so chunk-store
// rewrites do not race the tick's in-flight ops.
type backgroundGCRootsProvider struct{}

func (*backgroundGCRootsProvider) VisitGCRoots(_ context.Context, _ string, _ func(hash.Hash) bool) error {
	return nil
}

// RunUnderGCSafepointKeeper invokes fn while registered with the
// backend's GCSafepointController. An in-flight GC sweep blocks at its
// pre-finalize safepoint until SessionCommandEnd fires for the
// keeper's rootsProvider; new GC sweeps started while the bracket is
// active will likewise wait. Implements backends.GCSafepointBackend.
func (b *Backend) RunUnderGCSafepointKeeper(_ context.Context, fn func() error) error {
	if b.gcController == nil || b.backgroundRP == nil {
		return fn()
	}
	if err := b.gcController.SessionCommandBegin(b.backgroundRP); err != nil {
		return fmt.Errorf("RunUnderGCSafepointKeeper: SessionCommandBegin: %w", err)
	}
	defer b.gcController.SessionCommandEnd(b.backgroundRP)
	return fn()
}

// sessionAwareSafepoint mirrors dprocedures.sessionAwareSafepointController.
// The structure is copied (~30 lines) rather than extracted from dolt
// per the GC design doc: the controller is small and stable; extracting
// it would require lifting it out of a package that imports the SQL
// engine; dumbo has slightly different lifecycle requirements that
// would clutter a parameterized version.
//
// callSession is the GC requester; the Waiter excludes it from the
// waited set (the requester visits its own roots synchronously in
// BeginGC). Other registered sessions (wire request brackets and
// the backend's backgroundRP) are visited at safepoint resolution.
type sessionAwareSafepoint struct {
	controller  *gcctx.GCSafepointController
	callSession *dsess.DoltSession
	doltDB      *doltdb.DoltDB
	dbname      string
	keeper      func(hash.Hash) bool
	waiter      *gcctx.GCSafepointWaiter
}

func (sc *sessionAwareSafepoint) visit(ctx context.Context, sess gcctx.GCRootsProvider) error {
	return sess.VisitGCRoots(ctx, sc.dbname, sc.keeper)
}

func (sc *sessionAwareSafepoint) BeginGC(ctx context.Context, keeper func(hash.Hash) bool) error {
	sc.doltDB.PurgeCaches()
	sc.keeper = keeper
	if err := sc.visit(ctx, sc.callSession); err != nil {
		return err
	}
	sc.waiter = sc.controller.Waiter(ctx, sc.callSession, sc.visit)
	return nil
}

func (sc *sessionAwareSafepoint) EstablishPreFinalizeSafepoint(ctx context.Context) error {
	return sc.waiter.Wait(ctx)
}

func (sc *sessionAwareSafepoint) EstablishPostFinalizeSafepoint(_ context.Context) error {
	return nil
}

func (sc *sessionAwareSafepoint) CancelSafepoint() {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = sc.waiter.Wait(canceled)
}

// gcSizeAndCount returns the byte size and chunk count of state's
// underlying GenerationalNBS.
func gcSizeAndCount(ctx context.Context, state *dbState) (uint64, uint32, error) {
	size, err := state.cs.Size(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("gcSizeAndCount: Size: %w", err)
	}
	count, err := state.cs.Count(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("gcSizeAndCount: Count: %w", err)
	}
	return size, count, nil
}

// DumboDBGC runs garbage collection on the database's chunk store.
// The target database is resolved from params.DBName via
// SplitRevisionDbName (any branch selector is stripped); a single
// chunk store covers every branch in that logical database.
//
// Requires a session in ctx. The session is the GC's callSession
// (excluded from the Waiter's waited set); a calling path with no
// session has no GCRootsProvider for BeginGC to start the root walk.
func (b *Backend) DumboDBGC(ctx context.Context, params *backends.GCParams) (*backends.GCResult, error) {
	sess := sessionFromContext(ctx)
	if sess == nil {
		return nil, fmt.Errorf("DumboDBGC: no session in context")
	}
	baseName, _ := doltdb.SplitRevisionDbName(params.DBName)
	if baseName == "" {
		return nil, fmt.Errorf("DumboDBGC: empty database name")
	}
	state, ok := b.lookupDbStateForDsess(baseName)
	if !ok {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBGC: database %q does not exist", baseName))
	}

	mode := strings.ToLower(strings.TrimSpace(params.Mode))
	if mode == "" {
		mode = "default"
	}
	var gcMode chunks.GCMode
	switch mode {
	case "default":
		gcMode = chunks.GCMode_Default
	case "full":
		gcMode = chunks.GCMode_Full
	default:
		return nil, fmt.Errorf("DumboDBGC: unknown mode %q (want \"default\" or \"full\")", params.Mode)
	}

	sizeBefore, countBefore, err := gcSizeAndCount(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("DumboDBGC: snapshotting pre-GC size: %w", err)
	}

	sc := &sessionAwareSafepoint{
		controller:  b.gcController,
		callSession: sess,
		doltDB:      state.doltDB,
		dbname:      baseName,
	}

	gcConfig := chunks.GCConfig{
		Mode:         gcMode,
		ArchiveLevel: chunks.SimpleArchive,
	}

	start := time.Now()
	if err := state.doltDB.GC(ctx, gcConfig, types.GCSafepointController(sc)); err != nil {
		return nil, fmt.Errorf("DumboDBGC: %w", err)
	}
	durationMs := time.Since(start).Milliseconds()

	sizeAfter, countAfter, err := gcSizeAndCount(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("DumboDBGC: snapshotting post-GC size: %w", err)
	}

	return &backends.GCResult{
		DB:           baseName,
		Mode:         mode,
		DurationMs:   durationMs,
		SizeBefore:   sizeBefore,
		SizeAfter:    sizeAfter,
		ChunksBefore: countBefore,
		ChunksAfter:  countAfter,
	}, nil
}
