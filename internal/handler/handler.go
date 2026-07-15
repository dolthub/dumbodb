// Copyright 2021 FerretDB Inc.
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

// Package handler provides a universal handler implementation for all backends.
package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/decorators/oplog"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/clientconn/cursor"
	"github.com/dolthub/dumbodb/internal/handler/users"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/ctxutil"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/logging"
	"github.com/dolthub/dumbodb/internal/util/must"
	"github.com/dolthub/dumbodb/internal/util/password"
	"github.com/dolthub/dumbodb/internal/util/state"
)

const (
	maxWriteBatchSize = int32(100000)

	// Required by C# driver for `IsMaster` and `hello` op reply, without it `DPANIC` is thrown.
	connectionID = int32(42)

	logicalSessionTimeoutMinutes = int32(30)
)

// Handler provides a set of methods to process clients' requests sent over wire protocol.
//
// MsgXXX methods handle OP_MSG commands.
// CmdQuery handles a limited subset of OP_QUERY messages.
//
// Handler instance is shared between all client connections.
type Handler struct {
	*NewOpts

	b backends.Backend

	cursors    *cursor.Registry
	commands   map[string]*Command
	paramStore *parameterStore
	wg         sync.WaitGroup
	processID  types.ObjectID

	cappedCleanupStop chan struct{}
}

// NewOpts represents handler configuration.
//
//nolint:vet // for readability
type NewOpts struct {
	Backend     backends.Backend
	TCPHost     string
	ReplSetName string

	SetupDatabase string
	SetupUsername string
	SetupPassword password.Password
	SetupTimeout  time.Duration

	L             *slog.Logger
	StateProvider *state.Provider

	// test options
	DisablePushdown         bool
	EnableNestedPushdown    bool
	CappedCleanupInterval   time.Duration
	CappedCleanupPercentage uint8
	EnableNewAuth           bool
	BatchSize               int
	MaxBsonObjectSizeBytes  int
}

func New(opts *NewOpts) (*Handler, error) {
	if opts.CappedCleanupPercentage == 0 {
		opts.CappedCleanupPercentage = 10
	}

	if opts.CappedCleanupPercentage >= 100 || opts.CappedCleanupPercentage <= 0 {
		return nil, fmt.Errorf(
			"percentage of documents to cleanup must be in range (0, 100), but %d given",
			opts.CappedCleanupPercentage,
		)
	}

	if opts.MaxBsonObjectSizeBytes == 0 {
		opts.MaxBsonObjectSizeBytes = types.MaxDocumentLen
	}

	if opts.BatchSize == 0 {
		opts.BatchSize = int(maxWriteBatchSize)
	}

	b := oplog.NewBackend(opts.Backend, logging.WithName(opts.L, "oplog"))

	h := &Handler{
		b:         b,
		NewOpts:   opts,
		cursors:   cursor.NewRegistry(logging.WithName(opts.L, "cursors")),
		processID: types.NewObjectID(),

		cappedCleanupStop: make(chan struct{}),
	}

	if err := h.setup(); err != nil {
		h.Close()
		return nil, lazyerrors.Error(err)
	}

	h.initCommands()

	h.wg.Add(1)

	go func() {
		defer h.wg.Done()

		h.runCappedCleanup()
	}()

	return h, nil
}

// Setup creates initial database and user if needed.
func (h *Handler) setup() error {
	if h.SetupDatabase == "" {
		return nil
	}

	ctx, span := otel.Tracer("").Start(context.TODO(), "HandlerSetup")
	defer span.End()

	if h.SetupTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.SetupTimeout)
		defer cancel()
	}

	info := conninfo.New()
	info.SetBypassBackendAuth()

	ctx = conninfo.Ctx(ctx, info)

	l := logging.WithName(h.L, "setup")

	var retry int64

	for ctx.Err() == nil {
		_, err := h.b.Status(ctx, nil)
		if err == nil {
			break
		}

		l.DebugContext(ctx, "Status failed", logging.Error(err))

		retry++
		ctxutil.SleepWithJitter(ctx, time.Second, retry)
	}

	res, err := h.b.ListDatabases(ctx, &backends.ListDatabasesParams{Name: h.SetupDatabase})
	if err != nil {
		return lazyerrors.Error(err)
	}

	if len(res.Databases) > 0 {
		l.DebugContext(ctx, "Database already exists")
		return nil
	}

	l.InfoContext(
		ctx,
		"Setting up database and user",
		slog.String("database", h.SetupDatabase),
		slog.String("username", h.SetupUsername),
	)

	db, err := h.b.Database(h.SetupDatabase)
	if err != nil {
		return lazyerrors.Error(err)
	}

	// that's the only way to create a database
	if err = db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "setup"}); err != nil {
		return lazyerrors.Error(err)
	}

	if err = db.DropCollection(ctx, &backends.DropCollectionParams{Name: "setup"}); err != nil {
		return lazyerrors.Error(err)
	}

	err = users.CreateUser(ctx, h.b, &users.CreateUserParams{
		Database: h.SetupDatabase,
		Username: h.SetupUsername,
		Password: h.SetupPassword,
	})
	if err != nil {
		return lazyerrors.Error(err)
	}

	return nil
}

// runCappedCleanup calls capped collections cleanup function according to the given interval.
func (h *Handler) runCappedCleanup() {
	if h.CappedCleanupInterval <= 0 {
		h.L.Info("Capped collections cleanup disabled.")
		return
	}

	h.L.Info("Capped collections cleanup enabled.", slog.Duration("interval", h.CappedCleanupInterval))

	ticker := time.NewTicker(h.CappedCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := h.runCappedCleanupTick(context.Background()); err != nil {
				h.L.Error("Failed to cleanup capped collections.", logging.Error(err))
			}

		case <-h.cappedCleanupStop:
			h.L.Info("Capped collections cleanup stopped.")
			return
		}
	}
}

// runCappedCleanupTick wraps cleanupAllCappedCollections in the GC
// safepoint keeper bracket when the backend implements
// backends.GCSafepointBackend. The bracket ensures an in-flight GC
// sweep waits for the tick to release before chunk-store rewrites
// race the tick's writes.
func (h *Handler) runCappedCleanupTick(ctx context.Context) error {
	if gsb, ok := h.b.(backends.GCSafepointBackend); ok {
		return gsb.RunUnderGCSafepointKeeper(ctx, func() error {
			return h.cleanupAllCappedCollections(ctx)
		})
	}
	return h.cleanupAllCappedCollections(ctx)
}

// Close gracefully shutdowns handler.
// It should be called after listener closes all client connections and stops listening.
// SessionIsolation reports whether the underlying backend is running in
// --session-isolation mode. Used by the wire-dispatch layer to reject
// startTransaction.
func (h *Handler) SessionIsolation() bool {
	if sab, ok := h.b.(backends.SessionAwareBackend); ok {
		return sab.SessionIsolation()
	}
	return false
}

func (h *Handler) SessionRegistry() *sqlctx.SessionRegistry {
	if sab, ok := h.b.(backends.SessionAwareBackend); ok {
		return sab.SessionRegistry()
	}
	return nil
}

// AutoCommitBoundary commits each branch a write recorded on the connection
// during the just-finished command. Safe to call unconditionally: a no-op when
// nothing was recorded.
func (h *Handler) AutoCommitBoundary(ctx context.Context) error {
	ci := conninfo.GetIfPresent(ctx)
	if ci == nil {
		return nil
	}
	targets := ci.DrainAutoCommit()
	if len(targets) == 0 {
		return nil
	}
	ac, ok := h.b.(backends.AutoCommitBackend)
	if !ok {
		return nil
	}
	for _, t := range targets {
		if _, err := ac.AutoCommit(ctx, t.DB, t.Branch, t.Message); err != nil {
			return err
		}
	}
	return nil
}

// AbortPendingTransaction discards any per-connection pending overlay
// and marks the txn as aborted, so a subsequent commitTransaction
// returns NoSuchTransaction (251). Used when the wire layer rejects an
// in-txn DDL with OperationNotSupportedInTransaction (263).
func (h *Handler) AbortPendingTransaction(ctx context.Context) {
	ci := conninfo.Get(ctx)
	if sab, ok := h.b.(backends.SessionAwareBackend); ok {
		sab.OnTransactionAbort(ci.Owner())
	}
	ci.SetTxnAborted(true)
}

func (h *Handler) Close() {
	h.cursors.Close()
	close(h.cappedCleanupStop)
	h.wg.Wait()
}

// cleanupAllCappedCollections drops the given percent of documents from all capped collections.
func (h *Handler) cleanupAllCappedCollections(ctx context.Context) error {
	ctx, span := otel.Tracer("").Start(ctx, "HandlerCleanupAllCappedCollections")
	h.L.DebugContext(ctx, "cleanupAllCappedCollections: started", slog.Int("percentage", int(h.CappedCleanupPercentage)))

	start := time.Now()
	defer func() {
		span.End()
		h.L.DebugContext(ctx, "cleanupAllCappedCollections: finished", slog.Duration("duration", time.Since(start)))
	}()

	connInfo := conninfo.New()
	connInfo.SetBypassBackendAuth()
	ctx = conninfo.Ctx(ctx, connInfo)

	dbList, err := h.b.ListDatabases(ctx, nil)
	if err != nil {
		return lazyerrors.Error(err)
	}

	for _, dbInfo := range dbList.Databases {
		db, err := h.b.Database(dbInfo.Name)
		if err != nil {
			return lazyerrors.Error(err)
		}

		cList, err := db.ListCollections(ctx, nil)
		if err != nil {
			return lazyerrors.Error(err)
		}

		for _, cInfo := range cList.Collections {
			if !cInfo.Capped() {
				continue
			}

			deleted, bytesFreed, err := h.cleanupCappedCollection(ctx, db, &cInfo, false)
			if err != nil {
				if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
					backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
					continue
				}

				return lazyerrors.Error(err)
			}

			if deleted > 0 || bytesFreed > 0 {
				h.L.InfoContext(
					ctx,
					"Capped collection cleaned up",
					slog.String("db", dbInfo.Name),
					slog.String("collection", cInfo.Name),
					slog.Int("deleted", int(deleted)),
					slog.Int64("bytes_freed", bytesFreed),
				)
			}
		}
	}

	return nil
}

// cleanupCappedCollection drops a percent of documents from the given capped collection and compacts it.
func (h *Handler) cleanupCappedCollection(ctx context.Context, db backends.Database, cInfo *backends.CollectionInfo, force bool) (int32, int64, error) { //nolint:lll // for readability
	must.BeTrue(cInfo.Capped())

	var docsDeleted int32
	var bytesFreed int64
	var statsBefore, statsAfter *backends.CollectionStatsResult

	coll, err := db.Collection(cInfo.Name)
	if err != nil {
		return 0, 0, lazyerrors.Error(err)
	}

	statsBefore, err = coll.Stats(ctx, &backends.CollectionStatsParams{Refresh: true})
	if err != nil {
		return 0, 0, lazyerrors.Error(err)
	}

	h.L.DebugContext(
		ctx,
		"cleanupCappedCollection: stats before",
		slog.Int64("size_total", statsBefore.SizeTotal),
		slog.Int64("size_collection", statsBefore.SizeCollection),
		slog.Int64("count_documents", statsBefore.CountDocuments),
	)

	// In order to be more precise w.r.t number of documents getting dropped and to avoid
	// deleting too many documents unnecessarily,
	//
	// - First, drop the surplus documents, if document count exceeds capped configuration.
	// - Collect stats again.
	// - If collection size still exceeds the capped size, then drop the documents based on
	//   CappedCleanupPercentage.

	if count := getDocCleanupCount(cInfo, statsBefore); count > 0 {
		err = deleteFirstNDocuments(ctx, coll, count)
		if err != nil {
			return 0, 0, lazyerrors.Error(err)
		}

		statsAfter, err = coll.Stats(ctx, &backends.CollectionStatsParams{Refresh: true})
		if err != nil {
			return 0, 0, lazyerrors.Error(err)
		}

		h.L.DebugContext(
			ctx,
			"cleanupCappedCollection: stats after document count reduction",
			slog.Int64("size_total", statsAfter.SizeTotal),
			slog.Int64("size_collection", statsAfter.SizeCollection),
			slog.Int64("count_documents", statsAfter.CountDocuments),
		)

		docsDeleted += int32(count)
		bytesFreed += (statsBefore.SizeTotal - statsAfter.SizeTotal)

		statsBefore = statsAfter
	}

	if count := getSizeCleanupCount(cInfo, statsBefore, h.CappedCleanupPercentage); count > 0 {
		err = deleteFirstNDocuments(ctx, coll, count)
		if err != nil {
			return 0, 0, lazyerrors.Error(err)
		}

		docsDeleted += int32(count)
	}

	if _, err = coll.Compact(ctx, &backends.CompactParams{Full: force}); err != nil {
		return 0, 0, lazyerrors.Error(err)
	}

	statsAfter, err = coll.Stats(ctx, &backends.CollectionStatsParams{Refresh: true})
	if err != nil {
		return 0, 0, lazyerrors.Error(err)
	}

	h.L.DebugContext(
		ctx,
		"cleanupCappedCollection: stats after compact",
		slog.Int64("size_total", statsAfter.SizeTotal),
		slog.Int64("size_collection", statsAfter.SizeCollection),
		slog.Int64("count_documents", statsAfter.CountDocuments),
	)

	bytesFreed += (statsBefore.SizeTotal - statsAfter.SizeTotal)

	// There's a possibility that the size of a collection might be greater at the
	// end of a compact operation if the collection is being actively written to at
	// the time of compaction.
	if bytesFreed < 0 {
		bytesFreed = 0
	}

	return docsDeleted, bytesFreed, nil
}

// getDocCleanupCount returns the number of documents to be deleted during capped collection cleanup
// based on document count of the collection and capped configuration.
func getDocCleanupCount(cInfo *backends.CollectionInfo, cStats *backends.CollectionStatsResult) int64 {
	if cInfo.CappedDocuments == 0 || cInfo.CappedDocuments >= cStats.CountDocuments {
		return 0
	}

	return (cStats.CountDocuments - cInfo.CappedDocuments)
}

// getSizeCleanupCount returns the number of documents to be deleted during capped collection cleanup
// based collection size, capped configuration and cleanup percentage.
func getSizeCleanupCount(cInfo *backends.CollectionInfo, cStats *backends.CollectionStatsResult, cleanupPercent uint8) int64 {
	if cInfo.CappedSize >= cStats.SizeCollection {
		return 0
	}

	return int64(float64(cStats.CountDocuments) * float64(cleanupPercent) / 100)
}

// deleteFirstNDocuments drops first n documents (based on order of insertion) from the collection.
func deleteFirstNDocuments(ctx context.Context, coll backends.Collection, n int64) error {
	if n == 0 {
		return nil
	}

	res, err := coll.Query(ctx, &backends.QueryParams{
		Sort:          must.NotFail(types.NewDocument("$natural", int64(1))),
		Limit:         n,
		OnlyRecordIDs: true,
	})
	if err != nil {
		return lazyerrors.Error(err)
	}

	defer res.Iter.Close()

	var recordIDs []int64

	for {
		var doc *types.Document

		_, doc, err = res.Iter.Next()
		if err != nil {
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			return lazyerrors.Error(err)
		}

		recordIDs = append(recordIDs, doc.RecordID())
	}

	if len(recordIDs) > 0 {
		_, err := coll.DeleteAll(ctx, &backends.DeleteAllParams{RecordIDs: recordIDs})
		if err != nil {
			return lazyerrors.Error(err)
		}
	}

	return nil
}
