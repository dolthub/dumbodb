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

// Package dolt provides a Dolt-backed storage backend for DumboDB.
//
// Storage hierarchy:
//   - One nbs.GenerationalNBS per MongoDB database, stored in <dataDir>/<dbName>/
//   - The NBS store root chunk is a StoreRoot flatbuffer (STRT)
//   - STRT embeds a refsAM inline: AddressMap mapping "heads/main" -> commitHash
//   - commitHash -> Commit (DCMT) with rootValue = RTVL (RootValue) chunk
//   - RTVL.tables wraps the collections AddressMap (ADRM) bytes inline
//   - Collections AddressMap (ADRM) maps collection names to prolly.Map root hashes
//   - Each prolly.Map uses key=ByteString(encoded MongoDB _id) and value=JSONAddr(JSON prolly tree hash)
//
// This layout is compatible with Dolt CLI tools (dolt log, dolt fsck, dolt status, etc.).
//
// Invariant: after init, HEAD stays at the "Initialize database" commit. Writes
// update only the working set (WRST). WRST.working_root_addr advances with each
// write; WRST.staged_root_addr stays equal to HEAD's rootValue until an explicit
// stage operation. `dolt status` shows "Changes not staged for commit" after writes.
//
// Branch parsing: the database name may contain an '@' separator (e.g. mydb@main)
// to specify the branch, but currently all data lives in a single NBS store per
// logical database name.
package dolt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	fb "github.com/dolthub/flatbuffers/v23/go"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/libraries/doltcore/commitgraph"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	"github.com/dolthub/dolt/go/libraries/doltcore/dsess"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/writer"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
)

const (
	// defaultSessionTimeout matches MongoDB's logicalSessionTimeoutMinutes default.
	defaultSessionTimeout     = 30 * time.Minute
	defaultSessionSweepPeriod = time.Minute

	// defaultDroppedDatabaseTTL is how long a preserved drop is kept before the GC removes it.
	defaultDroppedDatabaseTTL = 30 * 24 * time.Hour
	defaultDroppedGCPeriod    = time.Hour

	// defaultMemTableSize is the in-memory table size for NBS.
	defaultMemTableSize = 128 * 1024 * 1024

	// mainDataset is the dataset ID used for the "refs/heads/main" branch.
	// Dolt expects the full ref path including "refs/" prefix.
	mainDataset = "refs/heads/main"

	// workingSetDataset is the dataset ID for the working set.
	// Dolt derives this from WorkingSetRefForHead: "workingSets/" + "heads/main".
	// Required for `dolt status` to work without panicking.
	workingSetDataset = "workingSets/heads/main"

	// defaultBranch is the name of the default branch. All new databases start
	// with this branch, and connections without an explicit rootish default to it.
	defaultBranch = "main"

	// dbBranchSep is the separator between the database name and rootish in an
	// encoded database name (e.g. "mydb@main", "mydb@feature/foo"). The '@'
	// character is reserved as the delimiter and is forbidden in raw database
	// and raw branch names.
	dbBranchSep = "@"
)

type viewMeta struct {
	ViewOn    string
	Pipeline  *types.Array
	Collation *types.Document
}

// dbState holds the open Dolt store for a single MongoDB database.
type dbState struct {
	backend *Backend // back-pointer; lets helpers read backend-level config (e.g. sessionIsolation)
	name    string   // user-visible db name; needed to build qualified names for dsess

	mu    sync.RWMutex
	dbDir string

	cs *nbs.GenerationalNBS
	ns tree.NodeStore
	vs *dolttypes.ValueStore

	// doltDB is the primary handle for working-set and commit operations.
	// datasDB is the lower-level dataset interface required for operations not yet
	// exposed on *doltdb.DoltDB (GetDataset, Commit, Delete, Datasets).
	doltDB  *doltdb.DoltDB
	datasDB datas.Database

	// branchWSMu guards the structure of branchWS (insert only; entries
	// are never removed). Each entry has its own RWMutex guarding its
	// ws and wsHash fields. See branch_ws.go.
	branchWSMu sync.RWMutex
	branchWS   map[string]*branchWS

	collSchemaHash hash.Hash
	mergeState     *mergeInProgress
}

// getOrInitBranchAM is a bridge for version-control operations that still work
// in AM terms. Extracts the AM from the branch's working root.
// The caller must hold state.mu (write lock).
func (s *dbState) getOrInitBranchAM(ctx context.Context, branch string) (prolly.AddressMap, error) {
	ws, err := s.getOrInitBranchWS(ctx, branch)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	return amFromWorkingRoot(ctx, ws.WorkingRoot(), s.ns)
}

func amToRootValue(ctx context.Context, db *dbState, am prolly.AddressMap) (doltdb.RootValue, error) {
	rtvlMsg := buildRootValueFlatbuffer(am)
	return doltdb.NewRootValue(ctx, db.doltDB.ValueReadWriter(), db.doltDB.NodeStore(), dolttypes.SerialMessage(rtvlMsg))
}

// persistAM sets the working root for branch to am and immediately flushes to
// the doltDB working set. Used by version-control operations that produce a new
// AM and need it durable. The caller must hold s.mu (write lock).
func (s *dbState) persistAM(ctx context.Context, branch string, workingAM prolly.AddressMap) error {
	rtvlMsg := buildRootValueFlatbuffer(workingAM)
	rv, err := doltdb.NewRootValue(ctx, s.doltDB.ValueReadWriter(), s.doltDB.NodeStore(), dolttypes.SerialMessage(rtvlMsg))
	if err != nil {
		return err
	}
	var newWS *doltdb.WorkingSet
	if err := s.updateBranchWS(ctx, branch, func(cur *doltdb.WorkingSet) (*doltdb.WorkingSet, error) {
		// Set both working and staged to the same root so dolt sees a
		// clean working tree (no uncommitted diff). Conflict artifacts
		// are stored in the DTBL ArtifactMap, not in working/staged.
		newWS = cur.WithWorkingRoot(rv).WithStagedRoot(rv)
		return newWS, nil
	}); err != nil {
		return err
	}
	s.pushWSToSession(ctx, branch, newWS)
	return nil
}

// setAM is a bridge for version-control operations that still produce raw AMs.
// Wraps the AM in a RootValue and updates the branch working set in memory only;
// callers persist via persistAM or a separate updateWorkingSet path.
// The caller must hold s.mu (write lock).
func (s *dbState) setAM(ctx context.Context, branch string, am prolly.AddressMap) {
	rtvlMsg := buildRootValueFlatbuffer(am)
	rv, err := doltdb.NewRootValue(ctx, s.doltDB.ValueReadWriter(), s.doltDB.NodeStore(), dolttypes.SerialMessage(rtvlMsg))
	if err != nil {
		return
	}
	cur, err := s.loadBranchWS(ctx, branch)
	if err != nil {
		// Branch may not have an on-disk WS yet (e.g., a freshly
		// created branch). Build a minimal WS from HEAD.
		wsRef := doltref.NewWorkingSetRef("heads/" + branch)
		headRV, headErr := headRootValueForBranch(ctx, s, branch)
		if headErr != nil {
			headRV = rv
		}
		cur = doltdb.EmptyWorkingSet(wsRef).WithStagedRoot(headRV)
	}
	// Only update working root; staged stays where it was (HEAD until explicit stage).
	newWS := cur.WithWorkingRoot(rv)
	s.setBranchWS(branch, newWS)
	s.pushWSToSession(ctx, branch, newWS)
}

// pushWSToSession mirrors a side-channel WS update onto the calling
// session's branchState so VisitGCRoots sees the post-op state.
//
// The active-txn gate prevents idle sessions from accumulating
// dirty branchStates with stale WS snapshots, a left-over concern
// from the qsc.5 flusher era. The flusher is gone, but the gate
// stays: marking idle sessions dirty makes session.Commit / merge
// paths behave inconsistently for non-txn writes.
func (s *dbState) pushWSToSession(ctx context.Context, branch string, newWS *doltdb.WorkingSet) {
	if !dbNameDsessFriendly(s.name) || alwaysAutoCommit(s.name) {
		return
	}
	sess := sessionFromContext(ctx)
	if sess == nil || sess.GetTransaction() == nil {
		return
	}
	sqlCtx := sqlctx.Wrap(ctx, sess)
	qualified := qualifiedDbName(s.name, branch)
	_ = sess.SetWorkingSet(sqlCtx, qualified, newWS)
}

// Backend implements backends.Backend using Dolt storage.
type Backend struct {
	dataDir          string
	l                *slog.Logger
	autoCommit       bool // when true, each write auto-creates a Dolt commit
	sessionIsolation bool // when true, writes auto-fork into per-conn overlay and doltCommit merges

	mu  sync.RWMutex
	dbs map[string]*dbState // dbName -> dbState

	provider *dumbodbProvider

	gcController *gcctx.GCSafepointController
	// backgroundRP is a singleton GCRootsProvider used by
	// RunUnderGCSafepointKeeper to bracket background mutators
	// (capped cleanup, etc.). Stable per-Backend so the keeper's
	// per-session bookkeeping reuses the same entry across ticks.
	backgroundRP *backgroundGCRootsProvider
	sessions     *sqlctx.SessionRegistry

	docLocksMu sync.Mutex
	docLocks   map[string]*DocLockManager

	sweeperStop   chan struct{}
	sweeperDone   chan struct{}
	sweeperPeriod time.Duration

	droppedGCStop   chan struct{}
	droppedGCDone   chan struct{}
	droppedGCPeriod time.Duration
	droppedGCTTL    time.Duration
}

func docLocksKey(db, branch string) string {
	return db + "/" + branch
}

func (b *Backend) docLockManager(db, branch string) *DocLockManager {
	key := docLocksKey(db, branch)

	b.docLocksMu.Lock()
	defer b.docLocksMu.Unlock()

	if b.docLocks == nil {
		b.docLocks = map[string]*DocLockManager{}
	}
	m, ok := b.docLocks[key]
	if !ok {
		m = NewDocLockManager()
		b.docLocks[key] = m
	}
	return m
}

func (b *Backend) OnSessionEnd(owner string) {
	b.releaseLocksForOwner(owner)
	b.rollbackOwnerTxn(owner)
	// End() only sets lastUsed=0; calling PurgeNow here would deadlock
	// because OnSessionEnd runs inside the Shadow.Use that owns writeMu.
	// The 1-minute sweep tick reaps lastUsed=0 entries off the request
	// path.
	if b.sessions != nil {
		b.sessions.End(owner)
	}
}

func (b *Backend) OnTransactionCommit(ctx context.Context, owner string) error {
	sess := b.sessionForOwner(owner)
	if sess == nil {
		b.releaseLocksForOwner(owner)
		return nil
	}
	tx := sess.GetTransaction()
	if tx == nil {
		b.releaseLocksForOwner(owner)
		return nil
	}
	sqlCtx := sqlctx.Wrap(ctx, sess)

	b.mu.RLock()
	dbs := make([]*dbState, 0, len(b.dbs))
	for _, db := range b.dbs {
		dbs = append(dbs, db)
	}
	b.mu.RUnlock()

	var firstErr error
	for _, db := range dbs {
		db.mu.Lock()
		_, err := db.commitDirtyBranchesForSession(sqlCtx, sess, tx)
		db.mu.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// CommitWorkingSet (unlike CommitTransaction) doesn't reset ctx.Transaction.
	sqlCtx.SetTransaction(nil)

	b.releaseLocksForOwner(owner)
	return firstErr
}

func (b *Backend) OnTransactionAbort(owner string) {
	b.rollbackOwnerTxn(owner)
	b.releaseLocksForOwner(owner)
}

func (b *Backend) rollbackOwnerTxn(owner string) {
	sess := b.sessionForOwner(owner)
	if sess == nil {
		return
	}
	tx := sess.GetTransaction()
	if tx == nil {
		return
	}
	sqlCtx := sqlctx.Wrap(context.Background(), sess)
	_ = sess.Rollback(sqlCtx, tx)
	sqlCtx.SetTransaction(nil)
}

func (b *Backend) SessionIsolation() bool {
	return b.sessionIsolation
}

func (b *Backend) releaseLocksForOwner(owner string) {
	b.docLocksMu.Lock()
	managers := make([]*DocLockManager, 0, len(b.docLocks))
	for _, m := range b.docLocks {
		managers = append(managers, m)
	}
	b.docLocksMu.Unlock()

	for _, m := range managers {
		m.Release(owner)
	}
}

func (b *Backend) lookupDbStateForDsess(name string) (*dbState, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	state, ok := b.dbs[name]
	return state, ok
}

func (b *Backend) openDbNames() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.dbs))
	for name := range b.dbs {
		out = append(out, name)
	}
	return out
}

// parseAuthorString splits a "Name <email>" string into name and email.
// If the string doesn't contain " <", the whole string is used as both name and email.
func parseAuthorString(author string) (name, email string) {
	if idx := strings.Index(author, " <"); idx >= 0 {
		return author[:idx], strings.TrimSuffix(author[idx+2:], ">")
	}
	return author, author
}

// splitIdent splits a "Name <email>" string into name and email, defaulting the
// email to "name@dumbodb" when the string carries no address. This matches the
// historical commit-path fallback (distinct from parseAuthorString, which uses the
// whole string as the email).
func splitIdent(s string) (name, email string) {
	if idx := strings.Index(s, " <"); idx >= 0 {
		return s[:idx], strings.TrimSuffix(s[idx+2:], ">")
	}
	return s, s + "@dumbodb"
}

// committerOrAuthor returns committer when non-empty, else author.
func committerOrAuthor(author, committer string) string {
	if committer == "" {
		return author
	}
	return committer
}

// commitMetaAC builds commit metadata with distinct author and committer
// identities. When committer is empty it defaults to author (a freshly authored
// commit). authorDate sets the author date when non-zero; the committer date is
// captured when the commit is written.
func commitMetaAC(author, committer, desc string, authorDate time.Time) (*datas.CommitMeta, error) {
	an, ae := splitIdent(author)

	if committer == "" {
		committer = author
	}
	cn, ce := splitIdent(committer)

	aid := datas.CommitIdent{Name: an, Email: ae}
	if !authorDate.IsZero() {
		aid.Date = datas.CommitDateAt(authorDate)
	}

	return datas.NewCommitMetaWithAuthorCommitter(aid, datas.CommitIdent{Name: cn, Email: ce}, desc)
}

// NewBackend creates a new Dolt Backend, storing data under dataDir.
// When autoCommit is true, every document write (insert/update/delete) is
// automatically committed to Dolt history without an explicit doltCommit call.
// sessionTimeout overrides the default lsid-keyed session idle timeout
// (zero falls back to defaultSessionTimeout). sessionSweepPeriod overrides
// the registry sweep cadence (zero falls back to defaultSessionSweepPeriod).
func NewBackend(dataDir string, l *slog.Logger, autoCommit, sessionIsolation bool, sessionTimeout, sessionSweepPeriod time.Duration) (backends.Backend, error) {
	b, err := newBackend(dataDir, l, autoCommit, sessionIsolation, sessionTimeout, sessionSweepPeriod)
	if err != nil {
		return nil, err
	}
	return backends.BackendContract(b), nil
}

func newBackend(dataDir string, l *slog.Logger, autoCommit, sessionIsolation bool, sessionTimeout, sessionSweepPeriod time.Duration) (*Backend, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data directory: %w", err)
	}

	b := &Backend{
		dataDir:          dataDir,
		l:                l,
		autoCommit:       autoCommit,
		sessionIsolation: sessionIsolation,
		dbs:              make(map[string]*dbState),
		docLocks:         make(map[string]*DocLockManager),
		sweeperStop:      make(chan struct{}),
		sweeperDone:      make(chan struct{}),
		droppedGCStop:    make(chan struct{}),
		droppedGCDone:    make(chan struct{}),
		droppedGCPeriod:  defaultDroppedGCPeriod,
		droppedGCTTL:     defaultDroppedDatabaseTTL,
	}
	if sessionSweepPeriod > 0 {
		b.sweeperPeriod = sessionSweepPeriod
	} else {
		b.sweeperPeriod = defaultSessionSweepPeriod
	}

	b.gcController = gcctx.NewGCSafepointController()
	b.backgroundRP = &backgroundGCRootsProvider{}
	provider, err := newDumbodbProvider(dataDir, b.lookupDbStateForDsess, b.openDbNames, b.gcController)
	if err != nil {
		return nil, fmt.Errorf("constructing dsess provider: %w", err)
	}
	b.provider = provider
	if sessionTimeout <= 0 {
		sessionTimeout = defaultSessionTimeout
	}
	b.sessions = sqlctx.NewSessionRegistry(sessionTimeout, func(lsid string) (*dsess.DoltSession, error) {
		return sqlctx.NewSessionWithGC(b.provider, writer.NewWriteSession, b.gcController)
	})

	// Initialize the admin database so it always exists on disk, matching
	// MongoDB's behavior where the admin database is always present even when
	// empty. Without this, compact on a non-existent collection in admin
	// incorrectly returns "database does not exist" instead of "collection
	// does not exist".
	if _, err := b.getOrOpenDB(context.Background(), "admin", true); err != nil {
		return nil, fmt.Errorf("initializing admin database: %w", err)
	}

	go b.sessionSweepLoop()
	go b.droppedDatabaseGCLoop()

	return b, nil
}

func (b *Backend) sessionSweepLoop() {
	defer close(b.sweeperDone)

	ticker := time.NewTicker(b.sweeperPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-b.sweeperStop:
			return
		case <-ticker.C:
			if b.sessions != nil {
				b.sessions.Sweep(time.Now())
			}
		}
	}
}

func (b *Backend) Close() {
	if b.sweeperStop != nil {
		select {
		case <-b.sweeperStop:
		default:
			close(b.sweeperStop)
		}
		if b.sweeperDone != nil {
			<-b.sweeperDone
		}
	}

	if b.droppedGCStop != nil {
		select {
		case <-b.droppedGCStop:
		default:
			close(b.droppedGCStop)
		}
		if b.droppedGCDone != nil {
			<-b.droppedGCDone
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for name, db := range b.dbs {
		db.mu.Lock()
		if err := db.doltDB.Close(); err != nil {
			b.l.Error("closing database", "db", name, "err", err)
		}
		db.mu.Unlock()
	}

	b.dbs = make(map[string]*dbState)
}

func (b *Backend) Status(ctx context.Context, params *backends.StatusParams) (*backends.StatusResult, error) {
	sess := sessionFromContext(ctx)

	b.mu.RLock()
	defer b.mu.RUnlock()

	var totalCollections int64

	for dbName, db := range b.dbs {
		ws, err := db.loadBranchWS(ctx, defaultBranch)
		if err != nil {
			return nil, err
		}
		rv, err := workingRootViaSession(ctx, sess, ws, dbName, defaultBranch)
		if err != nil {
			return nil, err
		}
		names, err := rv.GetTableNames(ctx, "", false)
		if err != nil {
			return nil, err
		}
		totalCollections += int64(len(names))
	}

	return &backends.StatusResult{
		CountCollections: totalCollections,
	}, nil
}

// Database implements backends.Backend.
//
// name may be an encoded database name of the form "dbname@rootish" where
// rootish is a branch name, commit hash, tag name, or ancestor expression.
// The base db name and rootish are parsed here; collection reads use the
// rootish to load the historical RTVL when it is a commit hash or tag.
func (b *Backend) Database(name string) (backends.Database, error) {
	baseName, rootish := splitEncodedDBName(name)
	return backends.DatabaseContract(&database{
		backend: b,
		name:    baseName,
		rootish: rootish,
	}), nil
}

// splitEncodedDBName splits an encoded database name "dbname@rootish" into
// the base database name and rootish. If no '@' separator is present, the
// rootish defaults to "main" (the default branch).
//
// The rootish component is percent-decoded (RFC 3986 path encoding) so that
// branch names containing characters invalid in MongoDB database names (e.g. '.'
// in "v1.0", '/' in "feature/foo") can be encoded by the client as "v1%2E0" or
// "feature%2Ffoo". The handler has already validated the encoding before the
// backend is reached, so decode errors here fall back to the raw value.
//
// HEAD and HEAD~N are rejected at the handler level (rejectHEAD). DumboDB has
// no stateful "current branch" concept per connection.
//
// All-digit strings after '@' (e.g. Unix nanosecond timestamps) are not valid
// rootish expressions and cause the whole encoded name to be treated as a plain
// database name. This prevents spurious "not found as branch or tag" errors when
// client code accidentally produces database names like "prefix@1775505756999075683".
func splitEncodedDBName(encoded string) (dbName, rootish string) {
	if idx := strings.Index(encoded, dbBranchSep); idx > 0 {
		raw := encoded[idx+len(dbBranchSep):]
		candidate := raw
		if decoded, err := url.PathUnescape(raw); err == nil {
			candidate = decoded
		}
		// All-digit strings are not valid rootish expressions; treat the whole
		// encoded name as a plain database name instead.
		if !splitAllDigits(candidate) {
			// HEAD is rejected at the handler level (rejectHEAD).
			// If it reaches here, pass it through -- the handler will catch it.
			return encoded[:idx], candidate
		}
	}
	return encoded, defaultBranch
}

// splitAllDigits reports whether s consists entirely of ASCII decimal digits
// AND is not a valid Dolt commit hash length.
//
// Dolt commit hashes are exactly 32 base32 characters (0-9a-v); a 32-char
// all-digit string is a valid hash and must still be treated as a rootish.
// Any other all-digit string (e.g. a 19-digit UnixNano timestamp) cannot be
// a branch name, tag, hash, or ancestor expression.
func splitAllDigits(s string) bool {
	if s == "" || len(s) == 32 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (b *Backend) ListDatabases(ctx context.Context, params *backends.ListDatabasesParams) (*backends.ListDatabasesResult, error) {
	entries, err := os.ReadDir(b.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &backends.ListDatabasesResult{}, nil
		}

		return nil, err
	}

	var dbs []backends.DatabaseInfo

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Skip internal dot-prefixed dirs (e.g. preserved drops); not valid db names.
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		dbName := entry.Name()

		// System databases are always included, matching MongoDB's behavior where
		// admin, config, and local always appear in listDatabases regardless of
		// whether they contain user collections.
		isSystemDB := dbName == "admin" || dbName == "config" || dbName == "local"

		if !isSystemDB {
			state, err := b.getOrOpenDB(ctx, dbName, false)
			if err != nil {
				continue
			}

			if state == nil {
				continue
			}

			ws, wsErr := state.loadBranchWS(ctx, defaultBranch)
			if wsErr != nil {
				continue
			}
			rv, err := workingRootViaSession(ctx, sessionFromContext(ctx), ws, dbName, defaultBranch)
			if err != nil {
				continue
			}
			names, _ := rv.GetTableNames(ctx, "", false)
			if len(names) == 0 {
				continue
			}
		}

		if params != nil && params.Name != "" && dbName != params.Name {
			continue
		}

		dbs = append(dbs, backends.DatabaseInfo{Name: dbName})
	}

	slices.SortFunc(dbs, func(a, b backends.DatabaseInfo) int {
		return strings.Compare(a.Name, b.Name)
	})

	return &backends.ListDatabasesResult{Databases: dbs}, nil
}

func (b *Backend) DropDatabase(ctx context.Context, params *backends.DropDatabaseParams) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	dbDir := filepath.Join(b.dataDir, params.Name)

	if _, err := os.Stat(dbDir); os.IsNotExist(err) {
		return backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("database %q does not exist", params.Name))
	}

	if db, ok := b.dbs[params.Name]; ok {
		db.mu.Lock()
		_ = db.doltDB.Close()
		db.mu.Unlock()
		delete(b.dbs, params.Name)
	}

	// Soft delete: move (not remove) into the preserved-drops store so UndropDatabase can restore it.
	dest, err := b.preservedDest(params.Name)
	if err != nil {
		return fmt.Errorf("dropping database %q: %w", params.Name, err)
	}

	if err := os.Rename(dbDir, dest); err != nil {
		return fmt.Errorf("dropping database %q: %w", params.Name, err)
	}

	return nil
}

// getOrOpenDB returns the dbState for the given database name,
// opening/creating the NBS store if needed.
// If create is false and the directory doesn't exist, returns nil, nil.
// isReservedDatabase reports whether name is a system database DumboDB does not
// implement and must not let users create. admin is excluded (DumboDB uses it).
func isReservedDatabase(name string) bool {
	return name == "config" || name == "local"
}

func (b *Backend) getOrOpenDB(ctx context.Context, dbName string, create bool) (*dbState, error) {
	db, opened, err := b.getOrOpenDBLocked(ctx, dbName, create)
	if err != nil || db == nil {
		return db, err
	}

	// Register newly-opened mid-tx db's noms root with the active tx so
	// TransactionRoot lookups succeed. Outside b.mu because BaseDatabase
	// recurses into lookupDbStateForDsess.
	if opened {
		if sess := sessionFromContext(ctx); sess != nil {
			if tx, ok := sess.GetTransaction().(*dsess.DoltTransaction); ok {
				sqlCtx := sqlctx.Wrap(ctx, sess)
				if vdb, vok := b.provider.BaseDatabase(sqlCtx, dbName); vok {
					_ = tx.AddDb(sqlCtx, vdb)
				}
			}
		}
	}

	return db, nil
}

func (b *Backend) getOrOpenDBLocked(ctx context.Context, dbName string, create bool) (*dbState, bool, error) {
	b.mu.RLock()
	db, ok := b.dbs[dbName]
	b.mu.RUnlock()

	if ok {
		return db, false, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Double-check after acquiring write lock.
	if db, ok := b.dbs[dbName]; ok {
		return db, false, nil
	}

	dbDir := filepath.Join(b.dataDir, dbName)

	if !create {
		if _, err := os.Stat(dbDir); os.IsNotExist(err) {
			return nil, false, nil
		}
	}

	if create && isReservedDatabase(dbName) {
		return nil, false, backends.NewError(backends.ErrorCodeDatabaseNameIsInvalid,
			fmt.Errorf("database %q is a reserved system database and cannot be created", dbName))
	}

	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, false, fmt.Errorf("creating db directory for %q: %w", dbName, err)
	}

	q := nbs.NewUnlimitedMemQuotaProvider()

	newGenSt, err := nbs.NewLocalJournalingStore(ctx, dolttypes.Format_DOLT.VersionString(), dbDir, q, false, nil)
	if err != nil {
		return nil, false, fmt.Errorf("opening newgen NBS store for %q: %w", dbName, err)
	}

	oldgenDir := filepath.Join(dbDir, "oldgen")
	if err := os.MkdirAll(oldgenDir, 0o755); err != nil {
		_ = newGenSt.Close()
		return nil, false, fmt.Errorf("creating oldgen directory for %q: %w", dbName, err)
	}

	oldGenSt, err := nbs.NewLocalStore(ctx, newGenSt.Version(), oldgenDir, defaultMemTableSize, q, false)
	if err != nil {
		_ = newGenSt.Close()
		return nil, false, fmt.Errorf("opening oldgen NBS store for %q: %w", dbName, err)
	}

	ghostGen, err := nbs.NewGhostBlockStore(dbDir)
	if err != nil {
		_ = oldGenSt.Close()
		_ = newGenSt.Close()
		return nil, false, fmt.Errorf("opening ghost block store for %q: %w", dbName, err)
	}

	cs := nbs.NewGenerationalCS(oldGenSt, newGenSt, ghostGen)

	ns := tree.NewNodeStore(cs)

	// Inspect the existing root format before creating the datas.Database,
	// since datas.Database panics when reading an ADRM-format root.
	rootHash, err := cs.Root(ctx)
	if err != nil {
		_ = cs.Close()
		return nil, false, fmt.Errorf("reading root for %q: %w", dbName, err)
	}

	// Create the value store and higher-level DoltDB. datasDB is the low-level
	// datas.Database handle retained for dataset operations not yet on *doltdb.DoltDB.
	vs := dolttypes.NewValueStore(cs)
	doltDBVal, err := doltdb.DoltDBFromCS(cs, dbName)
	if err != nil {
		_ = cs.Close()
		return nil, false, fmt.Errorf("opening DoltDB for %q: %w", dbName, err)
	}
	doltDB := doltDBVal
	datasDB := doltdb.ExposeDatabaseFromDoltDB(doltDB)

	var am prolly.AddressMap

	if rootHash.IsEmpty() {
		am, err = prolly.NewEmptyAddressMap(ns)
		if err != nil {
			_ = doltDB.Close()
			return nil, false, fmt.Errorf("creating empty address map for %q: %w", dbName, err)
		}

		_, am, err = commitCollectionsAM(ctx, datasDB, datas.Dataset{}, am, "Initialize database")
		if err != nil {
			_ = doltDB.Close()
			return nil, false, fmt.Errorf("initial commit for %q: %w", dbName, err)
		}
	} else {
		rootChunk, err := cs.Get(ctx, rootHash)
		if err != nil {
			_ = doltDB.Close()
			return nil, false, fmt.Errorf("reading root chunk for %q: %w", dbName, err)
		}

		fileID := serial.GetFileID(rootChunk.Data())

		switch fileID {
		case serial.AddressMapFileID:
			// Legacy ADRM format: the root chunk is the collections AM directly.
			// Migrate to STRT by creating an initial dolt commit.
			b.l.Info("migrating database from ADRM to STRT root format", "db", dbName)

			amNode, _, err := tree.NodeFromChunk(&rootChunk)
			if err != nil {
				_ = doltDB.Close()
				return nil, false, fmt.Errorf("parsing ADRM root node for %q: %w", dbName, err)
			}

			am, err = prolly.NewAddressMap(amNode, ns)
			if err != nil {
				_ = doltDB.Close()
				return nil, false, fmt.Errorf("loading collections AM from ADRM root for %q: %w", dbName, err)
			}

			// Build the STRT structure manually because datas.Database panics on ADRM roots.
			// We need to do this atomically: write commit + STRT, then swap the NBS root.
			if err := migrateADRMtoSTRT(ctx, cs, vs, ns, am, rootHash); err != nil {
				_ = doltDB.Close()
				return nil, false, fmt.Errorf("migrating ADRM root for %q: %w", dbName, err)
			}

			// Now the NBS root is STRT; read the dataset from doltDB normally.
			mainDS, migErr := datasDB.GetDataset(ctx, mainDataset)
			if migErr != nil {
				_ = doltDB.Close()
				return nil, false, fmt.Errorf("getting dataset after migration for %q: %w", dbName, migErr)
			}
			_ = mainDS // used only to verify migration succeeded

		case serial.StoreRootFileID:
			// STRT format: read the collections AM from the head commit's rootValue.
			ds, err := datasDB.GetDataset(ctx, mainDataset)
			if err != nil {
				_ = doltDB.Close()
				return nil, false, fmt.Errorf("getting dataset for %q: %w", dbName, err)
			}

			if !ds.HasHead() {
				// Shouldn't happen for a valid STRT database, but recover gracefully.
				am, err = prolly.NewEmptyAddressMap(ns)
				if err != nil {
					_ = doltDB.Close()
					return nil, false, fmt.Errorf("creating empty address map for %q: %w", dbName, err)
				}
			} else {
				headValue, _, err := ds.MaybeHeadValue()
				if err != nil {
					_ = doltDB.Close()
					return nil, false, fmt.Errorf("reading head value for %q: %w", dbName, err)
				}

				headMsg, ok := headValue.(dolttypes.SerialMessage)
				if !ok {
					_ = doltDB.Close()
					return nil, false, fmt.Errorf("unexpected root value type %T for %q", headValue, dbName)
				}

				headFileID := serial.GetFileID([]byte(headMsg))
				switch headFileID {
				case serial.RootValueFileID:
					// RTVL format: prefer the working set  -- it holds the latest state
					// after writes that don't create commits (HEAD stays at last explicit
					// commit). Fall back to HEAD rootValue if the working set is missing.
					wsAM, wsErr := readAMFromWorkingSet(ctx, datasDB, cs, ns)
					if wsErr != nil {
						b.l.Warn("working set unavailable, falling back to HEAD AM", "db", dbName, "err", wsErr)
						rtvl, fallbackErr := serial.TryGetRootAsRootValue([]byte(headMsg), serial.MessagePrefixSz)
						if fallbackErr != nil {
							_ = doltDB.Close()
							return nil, false, fmt.Errorf("parsing RTVL for %q: %w", dbName, fallbackErr)
						}
						amNode, _, fallbackErr := tree.NodeFromBytes(rtvl.TablesBytes())
						if fallbackErr != nil {
							_ = doltDB.Close()
							return nil, false, fmt.Errorf("parsing collections AM from RTVL for %q: %w", dbName, fallbackErr)
						}
						wsAM, fallbackErr = prolly.NewAddressMap(amNode, ns)
						if fallbackErr != nil {
							_ = doltDB.Close()
							return nil, false, fmt.Errorf("loading collections AM from RTVL for %q: %w", dbName, fallbackErr)
						}
					}
					am = wsAM

				case serial.AddressMapFileID:
					// Legacy: commit rootValue is raw ADRM. Migrate to RTVL.
					b.l.Info("migrating database from ADRM-valued commit to RTVL", "db", dbName)

					amNode, _, err := tree.NodeFromBytes([]byte(headMsg))
					if err != nil {
						_ = doltDB.Close()
						return nil, false, fmt.Errorf("parsing ADRM from commit for %q: %w", dbName, err)
					}

					am, err = prolly.NewAddressMap(amNode, ns)
					if err != nil {
						_ = doltDB.Close()
						return nil, false, fmt.Errorf("loading collections AM from commit for %q: %w", dbName, err)
					}

					ds, am, err = commitCollectionsAM(ctx, datasDB, ds, am, "migrate: wrap collections AM in RTVL")
					if err != nil {
						_ = doltDB.Close()
						return nil, false, fmt.Errorf("RTVL migration commit for %q: %w", dbName, err)
					}

				default:
					_ = doltDB.Close()
					return nil, false, fmt.Errorf("unexpected head commit rootValue file ID %q for %q", headFileID, dbName)
				}
			}

		default:
			_ = doltDB.Close()
			return nil, false, fmt.Errorf("unexpected root chunk file ID %q for %q", fileID, dbName)
		}
	}

	wsRef := doltref.NewWorkingSetRef("heads/" + defaultBranch)
	mainWS, wsErr := doltDB.ResolveWorkingSet(ctx, wsRef)
	if wsErr != nil {
		rtvlMsg := buildRootValueFlatbuffer(am)
		rv, rvErr := doltdb.NewRootValue(ctx, doltDB.ValueReadWriter(), doltDB.NodeStore(), dolttypes.SerialMessage(rtvlMsg))
		if rvErr != nil {
			_ = doltDB.Close()
			return nil, false, fmt.Errorf("building initial root value for %q: %w", dbName, rvErr)
		}
		mainWS = doltdb.EmptyWorkingSet(wsRef).WithWorkingRoot(rv).WithStagedRoot(rv)
	}

	db = &dbState{
		backend:  b,
		name:     dbName,
		dbDir:    dbDir,
		cs:       cs,
		ns:       ns,
		vs:       vs,
		doltDB:   doltDB,
		datasDB:  datasDB,
		branchWS: make(map[string]*branchWS),
	}

	// Write the shared DSCH chunk once; the empty secondary-index AM is
	// memoized per-NodeStore in emptyIndexAMCache (see index_resolve.go).
	schemaMsg := buildCollectionTableSchema()
	schemaRef, err := vs.WriteValue(ctx, dolttypes.SerialMessage(schemaMsg))
	if err != nil {
		_ = doltDB.Close()
		return nil, false, fmt.Errorf("writing collection schema chunk: %w", err)
	}
	db.collSchemaHash = schemaRef.TargetHash()

	b.dbs[dbName] = db

	// If the working set was not on disk (new database or first open after migration),
	// persist it now so that dolt CLI tools can read it.
	if wsErr != nil {
		if persistErr := updateWorkingSet(ctx, doltDB, mainWS, defaultBranch); persistErr != nil {
			b.l.Warn("could not persist initial working set", "db", dbName, "err", persistErr)
		}
	}

	// Secondary indexes resolve per-collection from each branch's DTBL on
	// first read via the resolver in index_resolve.go; no eager hydration.
	// See docs/design/branch-scoped-index-metadata.md section 3.5.

	// Restore any in-progress merge/cherry-pick/rebase state persisted from a
	// previous server session. Errors are non-fatal: if the state file is corrupted
	// or the referenced chunks are gone, we log and proceed without merge state.
	if ms, loadErr := loadMergeState(ctx, db); loadErr != nil {
		b.l.Warn("could not restore merge state on open (treating as no merge in progress)",
			"db", dbName, "err", loadErr)
	} else if ms != nil {
		db.mergeState = ms
	}

	return db, true, nil
}

// commitCollectionsAM creates a new dolt commit with the given collections
// AddressMap as its root value, updating the "heads/main" dataset.
// Returns the updated dataset and the (unchanged) AM.
func commitCollectionsAM(ctx context.Context, datasDB datas.Database, ds datas.Dataset, am prolly.AddressMap, desc string) (datas.Dataset, prolly.AddressMap, error) {
	var err error
	if ds.ID() == "" {
		ds, err = datasDB.GetDataset(ctx, mainDataset)
		if err != nil {
			return datas.Dataset{}, am, err
		}
	}

	meta, err := datas.NewCommitMeta("dumbodb", "dumbodb@dumbodb", desc)
	if err != nil {
		return datas.Dataset{}, am, err
	}

	rtvlMsg := buildRootValueFlatbuffer(am)
	newDS, err := datasDB.Commit(ctx, ds, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta: meta,
	})
	if err != nil {
		return datas.Dataset{}, am, err
	}

	return newDS, am, nil
}

func (b *Backend) AutoCommit(ctx context.Context, dbName, branch, message, author string) (bool, error) {
	state, err := b.getOrOpenDB(ctx, dbName, false)
	if err != nil {
		return false, fmt.Errorf("AutoCommit: opening db %q: %w", dbName, err)
	}
	if state == nil {
		return false, nil
	}
	state.mu.RLock()
	defer state.mu.RUnlock()

	if state.mergeState != nil && state.mergeState.intoBranch == branch {
		return false, nil
	}
	return state.commitBranchWS(ctx, branch, message, author)
}

// commitCollectionsAMAs creates a new dolt commit with the given collections
// AddressMap as its root value, using the provided author name and timestamp.
// Returns the updated dataset and the (unchanged) AM.
func commitCollectionsAMAs(ctx context.Context, datasDB datas.Database, ds datas.Dataset, am prolly.AddressMap, desc, authorName, committerName string, ts time.Time) (datas.Dataset, prolly.AddressMap, error) {
	var err error
	if ds.ID() == "" {
		ds, err = datasDB.GetDataset(ctx, mainDataset)
		if err != nil {
			return datas.Dataset{}, am, err
		}
	}

	meta, err := commitMetaAC(authorName, committerName, desc, ts)
	if err != nil {
		return datas.Dataset{}, am, err
	}

	rtvlMsg := buildRootValueFlatbuffer(am)
	newDS, err := datasDB.Commit(ctx, ds, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta: meta,
	})
	if err != nil {
		return datas.Dataset{}, am, err
	}

	return newDS, am, nil
}

// migrateADRMtoSTRT converts a legacy ADRM-rooted NBS store to STRT format
// by writing an initial dolt commit and updating the NBS root atomically.
// Uses lower-level APIs to avoid datas.Database panicking on the ADRM root.
func migrateADRMtoSTRT(ctx context.Context, cs *nbs.GenerationalNBS, vs *dolttypes.ValueStore, ns tree.NodeStore, am prolly.AddressMap, adrmRootHash interface{ IsEmpty() bool }) error {
	oldRoot, err := cs.Root(ctx)
	if err != nil {
		return fmt.Errorf("reading current root: %w", err)
	}

	meta, err := datas.NewCommitMeta("dumbodb", "dumbodb@dumbodb", "migrate: ADRM to STRT")
	if err != nil {
		return fmt.Errorf("creating commit meta: %w", err)
	}

	// Create an initial commit with an RTVL-wrapped collections AM as its root value.
	// NewCommitForValue writes the root value chunk and builds the commit flatbuffer,
	// but does NOT write the commit chunk itself.
	rtvlMsg := buildRootValueFlatbuffer(am)
	commit, err := datas.NewCommitForValue(ctx, cs, vs, ns, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta: meta,
	})
	if err != nil {
		return fmt.Errorf("creating commit: %w", err)
	}

	commitRef, err := vs.WriteValue(ctx, commit.NomsValue())
	if err != nil {
		return fmt.Errorf("writing commit: %w", err)
	}

	refsAM, err := prolly.NewEmptyAddressMap(ns)
	if err != nil {
		return fmt.Errorf("creating refs address map: %w", err)
	}

	refsEditor := refsAM.Editor()
	if err := refsEditor.Add(ctx, mainDataset, commitRef.TargetHash()); err != nil {
		return fmt.Errorf("adding main ref: %w", err)
	}

	refsAM, err = refsEditor.Flush(ctx)
	if err != nil {
		return fmt.Errorf("flushing refs address map: %w", err)
	}

	strtMsg := buildStoreRootFlatbuffer(refsAM)

	strtRef, err := vs.WriteValue(ctx, dolttypes.SerialMessage(strtMsg))
	if err != nil {
		return fmt.Errorf("writing store root: %w", err)
	}

	ok, err := cs.Commit(ctx, strtRef.TargetHash(), oldRoot)
	if err != nil {
		return fmt.Errorf("committing new root: %w", err)
	}
	if !ok {
		return fmt.Errorf("root CAS failed (concurrent modification)")
	}

	return nil
}

// buildRootValueFlatbuffer builds an RTVL (RootValue) flatbuffer wrapping the
// given collections AddressMap as the tables field. The RTVL is the chunk type
// that dolt expects at commit.rootValue, working_root_addr, and staged_root_addr.
//
// Layout: feature_version=7, collation=utf8mb4_0900_bin, tables=ADRM bytes,
// foreign_key_addr=[0;20] (no foreign keys).
func buildRootValueFlatbuffer(am prolly.AddressMap) serial.Message {
	builder := fb.NewBuilder(256)
	amBytes := []byte(tree.ValueFromNode(am.Node()).(dolttypes.SerialMessage))
	tablesOff := builder.CreateByteVector(amBytes)
	var emptyFK [20]byte
	fkOff := builder.CreateByteVector(emptyFK[:])
	serial.RootValueStart(builder)
	serial.RootValueAddFeatureVersion(builder, 7) // DoltFeatureVersion
	serial.RootValueAddCollation(builder, serial.Collationutf8mb4_0900_bin)
	serial.RootValueAddTables(builder, tablesOff)
	serial.RootValueAddForeignKeyAddr(builder, fkOff)
	return serial.FinishMessage(builder, serial.RootValueEnd(builder), []byte(serial.RootValueFileID))
}

// buildStoreRootFlatbuffer builds a StoreRoot (STRT) flatbuffer with the
// given refsAM bytes embedded inline, replicating the unexported
// storeroot_flatbuffer() function from dolt/go/store/datas/refmap.go.
func buildStoreRootFlatbuffer(refsAM prolly.AddressMap) serial.Message {
	builder := fb.NewBuilder(1024)
	ambytes := []byte(tree.ValueFromNode(refsAM.Node()).(dolttypes.SerialMessage))
	voff := builder.CreateByteVector(ambytes)
	serial.StoreRootStart(builder)
	serial.StoreRootAddAddressMap(builder, voff)
	return serial.FinishMessage(builder, serial.StoreRootEnd(builder), []byte(serial.StoreRootFileID))
}

func workingSetForBranch(branch string) string {
	return "workingSets/heads/" + branch
}

// updateWorkingSet writes the working set with independent working and staged roots.
// This is required for `dolt status` to function  -- without a workingSets/heads/<branch>
// entry, dolt panics trying to read the working set.
//
// workingAM is the latest uncommitted state; stagedAM is what has been staged for
// the next commit (typically HEAD's rootValue until an explicit stage operation).
// The RTVL chunk for workingAM must already be in the value store (written by the
// caller via vs.WriteValue). The staged RTVL is recomputed from stagedAM and its
// chunk must also be present in the store (e.g. written by a prior commit).
func updateWorkingSet(ctx context.Context, ddb *doltdb.DoltDB, ws *doltdb.WorkingSet, branch string) error {
	wsRef := doltref.NewWorkingSetRef("heads/" + branch)
	// Use the current on-disk working set hash as the optimistic-lock prevHash.
	// ws.HashOf() returns an error for in-memory WS created via WithWorkingRoot,
	// so we resolve from disk instead.
	var prevHash hash.Hash
	if cur, resolveErr := ddb.ResolveWorkingSet(ctx, wsRef); resolveErr == nil {
		prevHash, _ = cur.HashOf()
	}
	meta := doltdb.TodoWorkingSetMeta()
	var rsc doltdb.ReplicationStatusController
	return ddb.UpdateWorkingSet(ctx, wsRef, ws, prevHash, meta, &rsc)
}

var _ backends.VersioningBackend = (*Backend)(nil)

// DumboDBCommit implements backends.VersioningBackend.
// It commits the current working set (collections AM) with the given message,
// author, and timestamp, creating a new dolt commit on the specified branch.
// If params.Branch is empty it defaults to "main".
func (b *Backend) DumboDBCommit(ctx context.Context, params *backends.CommitParams) (*backends.CommitResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBCommit: database %q does not exist", params.DBName))
	}

	message := params.Message
	if message == "" {
		message = "dolt commit"
	}

	ts := params.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	branch := params.Branch
	if branch == "" {
		branch = defaultBranch
	}

	if b.sessionIsolation {
		return b.doltCommitSessionIsolation(ctx, params, db, branch, message, ts)
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// Guard: reject dumboDBCommit during any in-progress merge, cherry-pick, or rebase.
	if db.mergeState != nil && db.mergeState.intoBranch == branch {
		if db.mergeState.hasUnresolvedConflicts() {
			if db.mergeState.isRebase {
				return nil, fmt.Errorf("dumboCommit: unresolved rebase conflicts remain")
			}
			if db.mergeState.isCherryPick {
				return nil, fmt.Errorf("dumboCommit: unresolved cherry-pick conflicts remain")
			}
			if db.mergeState.isRevert {
				return nil, fmt.Errorf("dumboCommit: unresolved revert conflicts remain")
			}
			return nil, fmt.Errorf("dumboCommit: unresolved merge conflicts remain")
		}
		if db.mergeState.isRebase {
			return nil, fmt.Errorf("dumboCommit: rebase in progress: use dumboRebase continue")
		}
		if db.mergeState.isCherryPick {
			return nil, fmt.Errorf("dumboCommit: cherry-pick in progress: use dumboCherryPick continue")
		}
		if db.mergeState.isRevert {
			return nil, fmt.Errorf("dumboCommit: revert in progress: use dumboRevert continue")
		}
		return nil, fmt.Errorf("dumboCommit: merge in progress: use dumboMerge continue")
	}

	if branch == defaultBranch {
		sess := sessionFromContext(ctx)
		fallbackWS, fbErr := db.loadBranchWS(ctx, defaultBranch)
		if fbErr != nil {
			return nil, fmt.Errorf("DumboDBCommit: loading WS for db %q: %w", params.DBName, fbErr)
		}
		if !params.AllowEmpty {
			headAM, err := db.headRootAM(ctx)
			if err != nil {
				return nil, fmt.Errorf("DumboDBCommit: reading HEAD AM for db %q: %w", params.DBName, err)
			}
			workingRV, rvErr := workingRootViaSession(ctx, sess, fallbackWS, params.DBName, defaultBranch)
			if rvErr != nil {
				return nil, fmt.Errorf("DumboDBCommit: reading working root for db %q: %w", params.DBName, rvErr)
			}
			workingAM, amErr := amFromWorkingRoot(ctx, workingRV, db.ns)
			if amErr != nil {
				return nil, fmt.Errorf("DumboDBCommit: reading working AM for db %q: %w", params.DBName, amErr)
			}
			if workingAM.HashOf() == headAM.HashOf() {
				return nil, backends.ErrEmptyCommit
			}
		}

		mainDS, dsErr := db.datasDB.GetDataset(ctx, mainDataset)
		if dsErr != nil {
			return nil, fmt.Errorf("DumboDBCommit: resolving main dataset for db %q: %w", params.DBName, dsErr)
		}
		workingRV, rvErr := workingRootViaSession(ctx, sess, fallbackWS, params.DBName, defaultBranch)
		if rvErr != nil {
			return nil, fmt.Errorf("DumboDBCommit: reading working root for db %q: %w", params.DBName, rvErr)
		}
		workingAM, amErr := amFromWorkingRoot(ctx, workingRV, db.ns)
		if amErr != nil {
			return nil, fmt.Errorf("DumboDBCommit: reading working AM for db %q: %w", params.DBName, amErr)
		}
		newDS, _, err := commitCollectionsAMAs(ctx, db.datasDB, mainDS, workingAM, message, params.Author, params.Committer, ts)
		if err != nil {
			return nil, fmt.Errorf("DumboDBCommit: committing db %q: %w", params.DBName, err)
		}

		if err := db.persistAM(ctx, defaultBranch, workingAM); err != nil {
			return nil, fmt.Errorf("DumboDBCommit: updating working set for %q: %w", params.DBName, err)
		}

		headHash, ok := newDS.MaybeHeadAddr()
		if !ok {
			return nil, fmt.Errorf("DumboDBCommit: no head after commit for db %q", params.DBName)
		}

		return &backends.CommitResult{
			CommitID:           headHash.String(),
			Branch:             branch,
			Message:            message,
			Author:             params.Author,
			Timestamp:          ts.UnixMilli(),
			Committer:          committerOrAuthor(params.Author, params.Committer),
			CommitterTimestamp: ts.UnixMilli(),
		}, nil
	}

	branchDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: resolving branch %q: %w", branch, err)
	}
	if !branchDS.HasHead() {
		return nil, fmt.Errorf("DumboDBCommit: branch %q has no commits", branch)
	}

	branchAM, err := db.getOrInitBranchAM(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: loading branch AM for %q: %w", branch, err)
	}

	if !params.AllowEmpty {
		branchHeadAM, err := headRootAMForBranch(ctx, db, branch)
		if err != nil {
			return nil, fmt.Errorf("DumboDBCommit: reading HEAD AM for branch %q: %w", branch, err)
		}
		if branchAM.HashOf() == branchHeadAM.HashOf() {
			return nil, backends.ErrEmptyCommit
		}
	}

	meta, err := commitMetaAC(params.Author, params.Committer, message, ts)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: building commit meta for branch %q: %w", branch, err)
	}

	rtvlMsg := buildRootValueFlatbuffer(branchAM)
	newDS, err := db.datasDB.Commit(ctx, branchDS, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{Meta: meta})
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: committing branch %q: %w", branch, err)
	}

	if err := db.persistAM(ctx, branch, branchAM); err != nil {
		return nil, fmt.Errorf("DumboDBCommit: updating working set for branch %q: %w", branch, err)
	}

	// Clear the cached branch WS so the next access reloads from the new HEAD.
	db.clearBranchWS(branch)

	headHash, ok := newDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("DumboDBCommit: no head after commit for branch %q", branch)
	}

	return &backends.CommitResult{
		CommitID:           headHash.String(),
		Branch:             branch,
		Message:            message,
		Author:             params.Author,
		Timestamp:          ts.UnixMilli(),
		Committer:          committerOrAuthor(params.Author, params.Committer),
		CommitterTimestamp: ts.UnixMilli(),
	}, nil
}

// DumboDBBranch implements backends.VersioningBackend.
//
// When params.Delete is false (default), it creates a new Dolt branch named
// params.Name, starting from the HEAD commit of the source branch params.From.
//
// When params.Delete is true, it deletes the branch named params.Name:
//   - Safe delete (Force=false, delete semantics): refuses if the branch HEAD is not
//     reachable from any other branch (i.e. data would be lost).
//   - Force delete (Force=true, forceDelete semantics): deletes unconditionally.
//
// Both branch names map to dataset IDs of the form "refs/heads/<name>".
func (b *Backend) DumboDBBranch(ctx context.Context, params *backends.BranchParams) (*backends.BranchResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBBranch: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBBranch: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if params.Delete {
		return dumboDBBranchDelete(ctx, db, params)
	}

	// Resolve From to a commit hash. From may be a branch name, commit hash, or
	// ancestor expression (e.g. "main~1"), so we use the general rootish resolver.
	headHash, err := resolveRootishToCommitHash(ctx, db, params.From)
	if err != nil {
		return nil, fmt.Errorf("DumboDBBranch: resolving source %q: %w", params.From, err)
	}

	newDatasetID := "refs/heads/" + params.Name
	newDS, err := db.datasDB.GetDataset(ctx, newDatasetID)
	if err != nil {
		return nil, fmt.Errorf("DumboDBBranch: getting new branch dataset %q: %w", params.Name, err)
	}
	if newDS.HasHead() {
		return nil, fmt.Errorf("DumboDBBranch: branch %q already exists", params.Name)
	}

	if _, err = db.datasDB.SetHead(ctx, newDS, headHash, ""); err != nil {
		return nil, fmt.Errorf("DumboDBBranch: creating branch %q: %w", params.Name, err)
	}

	// Eagerly create the working_set ref; dolt creates it lazily on
	// checkout, but session-isolation writes without checking out.
	branchRef := doltref.NewBranchRef(params.Name)
	if headCommit, hcErr := db.doltDB.ResolveCommitRef(ctx, branchRef); hcErr == nil {
		if rv, rvErr := headCommit.GetRootValue(ctx); rvErr == nil {
			wsRef := doltref.NewWorkingSetRef("heads/" + params.Name)
			emptyWS := doltdb.EmptyWorkingSet(wsRef).WithWorkingRoot(rv).WithStagedRoot(rv)
			_ = updateWorkingSet(ctx, db.doltDB, emptyWS, params.Name)
		}
	}

	// Refresh tx.dbStartPoints; otherwise subsequent writes to the new
	// branch hit "branch not found" against the pre-branch tx snapshot.
	if sess := sessionFromContext(ctx); sess != nil {
		if tx, ok := sess.GetTransaction().(*dsess.DoltTransaction); ok {
			sqlCtx := sqlctx.Wrap(ctx, sess)
			if vdb, vok := b.provider.BaseDatabase(sqlCtx, params.DBName); vok {
				_ = tx.AddDb(sqlCtx, vdb)
			}
		}
	}

	return &backends.BranchResult{Branch: params.Name}, nil
}

// dumboDBBranchDelete deletes the branch named params.Name.
// Caller must hold db.mu.Lock().
func dumboDBBranchDelete(ctx context.Context, db *dbState, params *backends.BranchParams) (*backends.BranchResult, error) {
	// Refuse to delete the current connection's branch.
	if params.Name == params.From {
		return nil, fmt.Errorf("DumboDBBranch: cannot delete the currently checked-out branch %q", params.Name)
	}

	datasetID := "refs/heads/" + params.Name
	branchDS, err := db.datasDB.GetDataset(ctx, datasetID)
	if err != nil {
		return nil, fmt.Errorf("DumboDBBranch: getting branch dataset %q: %w", params.Name, err)
	}
	if !branchDS.HasHead() {
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("DumboDBBranch: branch %q does not exist", params.Name))
	}

	branchHash, ok := branchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("DumboDBBranch: branch %q has no HEAD commit", params.Name)
	}

	if !params.Force {
		// Safe delete: check if branchHash is reachable from any other branch.
		// "Reachable" means branchHash is an ancestor of (or equal to) another
		// branch's HEAD.  We use FindCommonAncestor(branchCommit, otherCommit)
		// and compare the result to branchHash.
		branchCommit, loadErr := datas.LoadCommitAddr(ctx, db.vs, branchHash)
		if loadErr != nil {
			return nil, fmt.Errorf("DumboDBBranch: loading commit for branch %q: %w", params.Name, loadErr)
		}

		dsMap, dsErr := db.datasDB.Datasets(ctx)
		if dsErr != nil {
			return nil, fmt.Errorf("DumboDBBranch: listing datasets: %w", dsErr)
		}

		errFound := errors.New("reachable") // sentinel to stop IterAll early
		reachable := false
		iterErr := dsMap.IterAll(ctx, func(id string, headAddr hash.Hash) error {
			const prefix = "refs/heads/"
			if !strings.HasPrefix(id, prefix) {
				return nil
			}
			otherBranch := id[len(prefix):]
			if otherBranch == params.Name {
				return nil
			}
			otherCommit, loadErr := datas.LoadCommitAddr(ctx, db.vs, headAddr)
			if loadErr != nil {
				return nil // skip branches we can't load
			}
			baseHash, hasBase, caErr := datas.FindCommonAncestor(ctx, branchCommit, otherCommit, db.vs, db.vs, db.ns, db.ns)
			if caErr != nil || !hasBase {
				return nil
			}
			// branchHash is reachable from otherBranch when the common ancestor
			// equals branchHash (i.e. branch is an ancestor of or equal to other).
			if baseHash == branchHash {
				reachable = true
				return errFound
			}
			return nil
		})
		if iterErr != nil && !errors.Is(iterErr, errFound) {
			return nil, fmt.Errorf("DumboDBBranch: iterating datasets: %w", iterErr)
		}

		if !reachable {
			return nil, fmt.Errorf(
				"DumboDBBranch: branch %q has unmerged commits; use forceDelete to force delete",
				params.Name,
			)
		}
	}

	// Delete the working set for this branch if it exists (best-effort).
	wsID := workingSetForBranch(params.Name)
	wsDS, wsErr := db.datasDB.GetDataset(ctx, wsID)
	if wsErr == nil && wsDS.HasHead() {
		_, _ = db.datasDB.Delete(ctx, wsDS, "")
	}

	if _, err = db.datasDB.Delete(ctx, branchDS, ""); err != nil {
		return nil, fmt.Errorf("DumboDBBranch: deleting branch %q: %w", params.Name, err)
	}

	db.clearBranchWS(params.Name)

	return &backends.BranchResult{Branch: params.Name}, nil
}

// DumboDBMerge implements backends.VersioningBackend.
//
// It merges the From branch into the Into branch of the specified database.
// Four cases are handled:
//
//   - Abort (Abort=true): discard the in-progress merge and restore the pre-merge state.
//   - Already up-to-date: From's HEAD is an ancestor of (or equal to) Into's HEAD.
//   - Fast-forward: Into's HEAD is an ancestor of From's HEAD; the Into pointer is
//     simply advanced to From's HEAD without creating a new commit.
//   - True 3-way merge: a merge commit is created on the Into branch with both
//     branch HEADs as parents. When document-level conflicts exist, the merge is staged
//     but not committed; a *backends.MergeConflictError is returned and the caller must
//     resolve conflicts via DumboDBResolveConflict before DumboDBCommit will succeed.
func (b *Backend) DumboDBMerge(ctx context.Context, params *backends.MergeParams) (*backends.MergeResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBMerge: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if params.Abort {
		if db.mergeState == nil {
			return nil, fmt.Errorf("DumboDBMerge: no merge in progress to abort")
		}
		if db.mergeState.isCherryPick {
			return nil, fmt.Errorf("DumboDBMerge: cherry-pick in progress on branch %q; use dumboCherryPick abort instead", params.Into)
		}
		ms := db.mergeState
		db.mergeState = nil

		db.setAM(ctx, ms.intoBranch, ms.premergeAM)
		_ = clearMergeState(db) // best-effort: ignore error on abort

		return &backends.MergeResult{Message: "merge aborted"}, nil
	}

	if params.Continue {
		if db.mergeState == nil || db.mergeState.intoBranch != params.Into {
			return nil, fmt.Errorf("dumboMerge: no merge in progress")
		}
		if db.mergeState.isCherryPick {
			return nil, fmt.Errorf("DumboDBMerge: cherry-pick in progress on branch %q; use dumboCherryPick continue instead", params.Into)
		}
		if db.mergeState.hasUnresolvedConflicts() {
			return nil, fmt.Errorf("dumboMerge: unresolved merge conflicts remain")
		}
		ms := db.mergeState

		if newConflicts, reErr := b.recheckCrossValidation(ctx, db, ms); reErr != nil {
			return nil, fmt.Errorf("DumboDBMerge: continue: %w", reErr)
		} else if newConflicts {
			db.setAM(ctx, ms.intoBranch, ms.resolvedAM)
			if wsErr := persistConflictState(ctx, db, ms); wsErr != nil {
				return nil, fmt.Errorf("DumboDBMerge: continue: persisting new validation conflicts: %w", wsErr)
			}
			return nil, &backends.MergeConflictError{Conflicts: ms.summaries()}
		}

		intoBranchDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+ms.intoBranch)
		if err != nil {
			return nil, fmt.Errorf("DumboDBMerge: continue: resolving branch %q: %w", ms.intoBranch, err)
		}

		// Clear artifact maps from all conflicting collections before committing.
		if clearErr := clearConflictArtifacts(ctx, db, ms); clearErr != nil {
			return nil, fmt.Errorf("DumboDBMerge: continue: clearing artifacts: %w", clearErr)
		}

		contAM, amErr := b.currentWorkingAM(ctx, db, ms.intoBranch)
		if amErr != nil {
			return nil, fmt.Errorf("DumboDBMerge: continue: %w", amErr)
		}
		mergeRes, err := b.commitMerge(ctx, db, ms.fromBranch, ms.intoBranch, intoBranchDS, ms.intoHash, ms.fromHash, contAM, params.Message, params.Author, params.Committer)
		if err != nil {
			return nil, fmt.Errorf("DumboDBMerge: continue: %w", err)
		}

		db.mergeState = nil
		_ = clearMergeState(db) // best-effort
		return mergeRes, nil
	}

	// Guard: reject new merge initiation if a merge or cherry-pick is already in progress.
	if db.mergeState != nil {
		switch {
		case db.mergeState.isRebase:
			return nil, fmt.Errorf("DumboDBMerge: rebase in progress on branch %q; resolve conflicts or abort first", params.Into)
		case db.mergeState.isCherryPick:
			return nil, fmt.Errorf("DumboDBMerge: cherry-pick in progress on branch %q; resolve conflicts or abort first", params.Into)
		case db.mergeState.isRevert:
			return nil, fmt.Errorf("DumboDBMerge: revert in progress on branch %q; resolve conflicts or abort first", params.Into)
		default:
			return nil, fmt.Errorf("DumboDBMerge: merge already in progress on branch %q; resolve conflicts or abort first", params.Into)
		}
	}

	intoBranchDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+params.Into)
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: resolving into branch %q: %w", params.Into, err)
	}
	if !intoBranchDS.HasHead() {
		return nil, fmt.Errorf("DumboDBMerge: into branch %q has no commits", params.Into)
	}
	intoHash, ok := intoBranchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("DumboDBMerge: into branch %q has no head address", params.Into)
	}

	fromBranchDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+params.From)
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: resolving from branch %q: %w", params.From, err)
	}
	if !fromBranchDS.HasHead() {
		return nil, fmt.Errorf("DumboDBMerge: from branch %q has no commits", params.From)
	}
	fromHash, ok := fromBranchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("DumboDBMerge: from branch %q has no head address", params.From)
	}

	intoCommit, err := datas.LoadCommitAddr(ctx, db.vs, intoHash)
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: loading into commit: %w", err)
	}
	fromCommit, err := datas.LoadCommitAddr(ctx, db.vs, fromHash)
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: loading from commit: %w", err)
	}

	baseHash, hasBase, err := datas.FindCommonAncestor(ctx, intoCommit, fromCommit, db.vs, db.vs, db.ns, db.ns)
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: finding common ancestor: %w", err)
	}
	if !hasBase {
		return nil, fmt.Errorf("DumboDBMerge: branches %q and %q have no common ancestor", params.Into, params.From)
	}

	// Already up-to-date: From's HEAD is an ancestor of (or equal to) Into's HEAD.
	if baseHash == fromHash {
		return &backends.MergeResult{
			CommitID: intoHash.String(),
			Message:  "already up-to-date",
		}, nil
	}

	// FFOnly: fail if a fast-forward is not possible (i.e. branches have diverged).
	if params.FFOnly && baseHash != intoHash {
		return nil, fmt.Errorf("DumboDBMerge: not possible to fast-forward")
	}

	// Fast-forward: Into's HEAD is an ancestor of From's HEAD.
	if baseHash == intoHash && !params.NoFF {
		if _, ffErr := db.datasDB.SetHead(ctx, intoBranchDS, fromHash, ""); ffErr != nil {
			return nil, fmt.Errorf("DumboDBMerge: fast-forward: advancing branch pointer: %w", ffErr)
		}
		// Update WS for any Into branch, not just defaultBranch: the
		// eager working_set ref would otherwise stay at the pre-FF root.
		ffAM, ffAMErr := amFromCommitHash(ctx, db, fromHash.String())
		if ffAMErr != nil {
			return nil, fmt.Errorf("DumboDBMerge: fast-forward: loading AM: %w", ffAMErr)
		}
		db.setAM(ctx, params.Into, ffAM)
		if err := db.persistAM(ctx, params.Into, ffAM); err != nil {
			return nil, fmt.Errorf("DumboDBMerge: fast-forward: updating working set: %w", err)
		}
		return &backends.MergeResult{
			CommitID: fromHash.String(),
			Message:  "fast-forward",
		}, nil
	}

	// True 3-way merge (or forced non-fast-forward): load AddressMaps and attempt to merge.
	intoAM, err := amFromCommitHash(ctx, db, intoHash.String())
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: loading into AM: %w", err)
	}
	fromAM, err := amFromCommitHash(ctx, db, fromHash.String())
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: loading from AM: %w", err)
	}
	baseAM, err := amFromCommitHash(ctx, db, baseHash.String())
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: loading base AM: %w", err)
	}

	mergedAM, conflicts, viewConflicts, metaConflicts, err := mergeAddressMapsWithConflicts(ctx, db, intoAM, fromAM, baseAM, fromHash, baseHash,
		fmt.Sprintf("branch '%s' (ours)", params.Into), fmt.Sprintf("branch '%s' (theirs)", params.From))
	if err != nil {
		return nil, fmt.Errorf("DumboDBMerge: %w", err)
	}

	if len(conflicts) > 0 || len(viewConflicts) > 0 || len(metaConflicts) > 0 {
		// Capture the pre-merge working set AM for abort support.
		preMergeAM, err := db.getOrInitBranchAM(ctx, params.Into)
		if err != nil {
			return nil, fmt.Errorf("DumboDBMerge: loading premerge AM for branch %q: %w", params.Into, err)
		}

		db.mergeState = &mergeInProgress{
			fromBranch:    params.From,
			intoBranch:    params.Into,
			premergeAM:    preMergeAM,
			fromHash:      fromHash,
			intoHash:      intoHash,
			conflicts:     conflicts,
			viewConflicts: viewConflicts,
			metaConflicts: metaConflicts,
			resolvedAM:    mergedAM,
		}

		// Update the in-memory branch AM so reads during conflict resolution
		// reflect the merged state (right-only changes already applied).
		db.setAM(ctx, params.Into, mergedAM)

		if wsErr := persistConflictState(ctx, db, db.mergeState); wsErr != nil {
			db.mergeState = nil
			return nil, fmt.Errorf("DumboDBMerge: persisting conflict state: %w", wsErr)
		}

		summaries := db.mergeState.summaries()
		return nil, &backends.MergeConflictError{Conflicts: summaries}
	}

	// Clean merge  -- commit immediately.
	return b.commitMerge(ctx, db, params.From, params.Into, intoBranchDS, intoHash, fromHash, mergedAM, params.Message, params.Author, params.Committer)
}

// currentWorkingAM returns the collections AddressMap of branch's working root.
// Caller must hold state.mu.
func (b *Backend) currentWorkingAM(ctx context.Context, db *dbState, branch string) (prolly.AddressMap, error) {
	ws, err := db.loadBranchWS(ctx, branch)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("currentWorkingAM: loading WS for %q: %w", branch, err)
	}
	return amFromWorkingRoot(ctx, ws.WorkingRoot(), db.ns)
}

// commitMerge creates a merge commit on intoBranch with both branch HEADs as parents.
// Called for clean merges (no conflicts) and for continue (conflict-resolved) merges from DumboDBMerge.
// message and author are optional; if empty, defaults are used.
func (b *Backend) commitMerge(
	ctx context.Context,
	db *dbState,
	fromBranch, intoBranch string,
	intoBranchDS datas.Dataset,
	intoHash, fromHash hash.Hash,
	mergedAM prolly.AddressMap,
	message, author, committer string,
) (*backends.MergeResult, error) {
	mergeMessage := message
	if mergeMessage == "" {
		mergeMessage = fmt.Sprintf("Merge branch '%s' into '%s'", fromBranch, intoBranch)
	}

	if author == "" {
		author = "dumbodb <dumbodb@dumbodb>"
	}
	if committer == "" {
		committer = author
	}

	meta, err := commitMetaAC(author, committer, mergeMessage, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("commitMerge: building commit meta: %w", err)
	}

	rtvlMsg := buildRootValueFlatbuffer(mergedAM)
	newDS, err := db.datasDB.Commit(ctx, intoBranchDS, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta:    meta,
		Parents: []hash.Hash{intoHash, fromHash},
	})
	if err != nil {
		return nil, fmt.Errorf("commitMerge: committing merge: %w", err)
	}

	mergeHash, ok := newDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("commitMerge: no head after merge commit")
	}

	db.setAM(ctx, intoBranch, mergedAM)
	if intoBranch == defaultBranch {
		if err := db.persistAM(ctx, defaultBranch, mergedAM); err != nil {
			return nil, fmt.Errorf("commitMerge: updating working set: %w", err)
		}
	}

	an, ae := splitIdent(author)
	cn, ce := splitIdent(committer)
	return &backends.MergeResult{
		CommitID:           mergeHash.String(),
		Message:            mergeMessage,
		Author:             an + " <" + ae + ">",
		Timestamp:          time.Now().UnixMilli(),
		Committer:          cn + " <" + ce + ">",
		CommitterTimestamp: time.Now().UnixMilli(),
	}, nil
}

// DumboDBCherryPick implements backends.VersioningBackend.
//
// It applies the diff introduced by the named commit onto the current branch and
// creates a new single-parent commit. Three cases:
//
//   - Abort (Abort=true): discard the in-progress cherry-pick and restore the
//     pre-cherry-pick state.
//   - Continue (Continue=true): after conflict resolution, complete the cherry-pick
//     and create the new commit.
//   - Normal pick: resolve the commit to cherry-pick, use its parent as the base,
//     and perform a 3-way merge of (current HEAD, cherry-pick commit, parent of
//     cherry-pick commit). On conflict, stage the partial result and return
//     *backends.DumboDBCherryPickConflictError.
func (b *Backend) DumboDBCherryPick(ctx context.Context, params *backends.CherryPickParams) (*backends.CherryPickResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCherryPick: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBCherryPick: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	branch := params.Branch
	if branch == "" {
		branch = defaultBranch
	}

	if params.Abort {
		if db.mergeState == nil || !db.mergeState.isCherryPick {
			return nil, fmt.Errorf("DumboDBCherryPick: no cherry-pick in progress to abort")
		}
		ms := db.mergeState
		db.mergeState = nil

		db.setAM(ctx, ms.intoBranch, ms.premergeAM)
		_ = clearMergeState(db)

		return &backends.CherryPickResult{Message: "cherry-pick aborted"}, nil
	}

	if params.Continue {
		if db.mergeState == nil || !db.mergeState.isCherryPick || db.mergeState.intoBranch != branch {
			return nil, fmt.Errorf("DumboDBCherryPick: no cherry-pick in progress on branch %q", branch)
		}
		if db.mergeState.hasUnresolvedConflicts() {
			return nil, fmt.Errorf("DumboDBCherryPick: unresolved cherry-pick conflicts remain")
		}
		ms := db.mergeState

		intoBranchDS, dsErr := db.datasDB.GetDataset(ctx, "refs/heads/"+ms.intoBranch)
		if dsErr != nil {
			return nil, fmt.Errorf("DumboDBCherryPick: continue: resolving branch %q: %w", ms.intoBranch, dsErr)
		}

		if clearErr := clearConflictArtifacts(ctx, db, ms); clearErr != nil {
			return nil, fmt.Errorf("DumboDBCherryPick: continue: clearing artifacts: %w", clearErr)
		}

		// Load original commit metadata to preserve author identity.
		contPickCommit, contPickErr := datas.LoadCommitAddr(ctx, db.vs, ms.pickHash)
		if contPickErr != nil {
			return nil, fmt.Errorf("DumboDBCherryPick: continue: loading pick commit: %w", contPickErr)
		}
		contPickMeta, contMetaErr := datas.GetCommitMeta(ctx, contPickCommit.NomsValue())
		if contMetaErr != nil {
			return nil, fmt.Errorf("DumboDBCherryPick: continue: reading pick commit meta: %w", contMetaErr)
		}

		contAM, contAMErr := b.currentWorkingAM(ctx, db, ms.intoBranch)
		if contAMErr != nil {
			return nil, fmt.Errorf("DumboDBCherryPick: continue: %w", contAMErr)
		}
		pickRes, pickErr := b.commitCherryPick(ctx, db, ms.intoBranch, intoBranchDS, ms.intoHash, ms.pickHash, contAM, contPickMeta, ms.originalMsg, params.Message, params.Committer)
		if pickErr != nil {
			return nil, fmt.Errorf("DumboDBCherryPick: continue: %w", pickErr)
		}

		db.mergeState = nil
		_ = clearMergeState(db)
		return pickRes, nil
	}

	// Guard: reject new cherry-pick if a merge, cherry-pick, rebase, or revert is already in progress.
	if db.mergeState != nil {
		switch {
		case db.mergeState.isRebase:
			return nil, fmt.Errorf("DumboDBCherryPick: rebase in progress on branch %q; resolve conflicts or abort first", branch)
		case db.mergeState.isCherryPick:
			return nil, fmt.Errorf("DumboDBCherryPick: cherry-pick already in progress on branch %q; resolve conflicts or abort first", branch)
		case db.mergeState.isRevert:
			return nil, fmt.Errorf("DumboDBCherryPick: revert in progress on branch %q; resolve conflicts or abort first", branch)
		default:
			return nil, fmt.Errorf("DumboDBCherryPick: merge in progress on branch %q; resolve conflicts or abort first", branch)
		}
	}

	if params.Commit == "" {
		return nil, fmt.Errorf("DumboDBCherryPick: commit parameter is required")
	}

	pickHash, err := resolveRootishToCommitHash(ctx, db, params.Commit)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCherryPick: resolving commit %q: %w", params.Commit, err)
	}

	// Load the cherry-pick commit to read its message and find its parent.
	pickCommit, err := datas.LoadCommitAddr(ctx, db.vs, pickHash)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCherryPick: loading commit %q: %w", pickHash, err)
	}

	pickMeta, err := datas.GetCommitMeta(ctx, pickCommit.NomsValue())
	if err != nil {
		return nil, fmt.Errorf("DumboDBCherryPick: reading meta for commit %q: %w", pickHash, err)
	}
	originalMsg := pickMeta.Description

	// Get the parent hash of the commit to use as the merge base.
	parentAddrs, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, pickCommit.NomsValue().(dolttypes.SerialMessage))
	if err != nil {
		return nil, fmt.Errorf("DumboDBCherryPick: reading parents for commit %q: %w", pickHash, err)
	}

	// Load the base AM (parent of the cherry-picked commit).
	// For a root commit with no parent, use an empty AM as the base.
	var baseAM prolly.AddressMap
	var pickBaseHash hash.Hash // parent commit hash; zero-value if pick has no parent
	if len(parentAddrs) == 0 {
		baseAM, err = prolly.NewEmptyAddressMap(db.ns)
		if err != nil {
			return nil, fmt.Errorf("DumboDBCherryPick: creating empty base AM: %w", err)
		}
	} else {
		pickBaseHash = parentAddrs[0]
		baseAM, err = amFromCommitHash(ctx, db, parentAddrs[0].String())
		if err != nil {
			return nil, fmt.Errorf("DumboDBCherryPick: loading parent AM for commit %q: %w", pickHash, err)
		}
	}

	// Load the cherry-pick commit's AM (the "from" side).
	fromAM, err := amFromCommitHash(ctx, db, pickHash.String())
	if err != nil {
		return nil, fmt.Errorf("DumboDBCherryPick: loading pick AM for commit %q: %w", pickHash, err)
	}

	intoBranchDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCherryPick: resolving into branch %q: %w", branch, err)
	}
	if !intoBranchDS.HasHead() {
		return nil, fmt.Errorf("DumboDBCherryPick: into branch %q has no commits", branch)
	}
	intoHash, ok := intoBranchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("DumboDBCherryPick: into branch %q has no head address", branch)
	}

	// Load the current branch's HEAD AM (the "into" side).
	intoAM, err := amFromCommitHash(ctx, db, intoHash.String())
	if err != nil {
		return nil, fmt.Errorf("DumboDBCherryPick: loading into AM for branch %q: %w", branch, err)
	}

	// Perform the 3-way merge: apply cherry-pick diff (base->from) onto current HEAD (into).
	mergedAM, conflicts, viewConflicts, metaConflicts, err := mergeAddressMapsWithConflicts(ctx, db, intoAM, fromAM, baseAM, pickHash, pickBaseHash,
		fmt.Sprintf("branch '%s' (ours)", branch), fmt.Sprintf("commit '%s' (theirs)", pickHash.String()))
	if err != nil {
		return nil, fmt.Errorf("DumboDBCherryPick: %w", err)
	}

	if len(conflicts) > 0 || len(viewConflicts) > 0 || len(metaConflicts) > 0 {
		// Capture the pre-pick AM for abort support.
		prePickAM, err := db.getOrInitBranchAM(ctx, branch)
		if err != nil {
			return nil, fmt.Errorf("DumboDBCherryPick: loading pre-pick AM for branch %q: %w", branch, err)
		}

		db.mergeState = &mergeInProgress{
			intoBranch:    branch,
			premergeAM:    prePickAM,
			intoHash:      intoHash,
			conflicts:     conflicts,
			viewConflicts: viewConflicts,
			metaConflicts: metaConflicts,
			resolvedAM:    mergedAM,
			isCherryPick:  true,
			pickHash:      pickHash,
			originalMsg:   originalMsg,
		}

		if wsErr := persistConflictState(ctx, db, db.mergeState); wsErr != nil {
			db.mergeState = nil
			return nil, fmt.Errorf("DumboDBCherryPick: persisting conflict state: %w", wsErr)
		}

		summaries := db.mergeState.summaries()
		return nil, &backends.DumboDBCherryPickConflictError{Conflicts: summaries}
	}

	// Clean cherry-pick  -- commit immediately.
	return b.commitCherryPick(ctx, db, branch, intoBranchDS, intoHash, pickHash, mergedAM, pickMeta, originalMsg, params.Message, params.Committer)
}

// commitCherryPick creates a single-parent commit on the branch applying the cherry-picked AM.
// originalMsg is the cherry-picked commit's message; message (if non-empty) overrides it.
// pickMeta is the original commit's metadata (used to preserve author identity).
// committerOverride is optional; when set, uses it as the committer identity;
// when empty, committer equals the original author (no distinct committer).
func (b *Backend) commitCherryPick(
	ctx context.Context,
	db *dbState,
	branch string,
	branchDS datas.Dataset,
	intoHash, pickHash hash.Hash,
	pickedAM prolly.AddressMap,
	pickMeta *datas.CommitMeta,
	originalMsg, message, committerOverride string,
) (*backends.CherryPickResult, error) {
	commitMsg := message
	if commitMsg == "" {
		if originalMsg != "" {
			commitMsg = originalMsg + "\n\n(cherry picked from commit " + pickHash.String() + ")"
		} else {
			commitMsg = "cherry picked from commit " + pickHash.String()
		}
	}

	var meta *datas.CommitMeta
	var err error
	if committerOverride != "" {
		// Explicit committer: preserve original author, set distinct committer.
		origAuthor := datas.CommitIdent{Name: pickMeta.Author.Name, Email: pickMeta.Author.Email, Date: pickMeta.Author.Date}
		commitName, commitEmail := parseAuthorString(committerOverride)
		committer := datas.CommitIdent{Name: commitName, Email: commitEmail}
		meta, err = datas.NewCommitMetaWithAuthorCommitter(origAuthor, committer, commitMsg)
	} else {
		// No committer override: preserve original author; committer is implicit (same identity).
		meta, err = datas.NewCommitMetaWithAuthor(pickMeta.Author.Name, pickMeta.Author.Email, commitMsg, pickMeta.Author.Date.Time())
	}
	if err != nil {
		return nil, fmt.Errorf("commitCherryPick: building commit meta: %w", err)
	}

	rtvlMsg := buildRootValueFlatbuffer(pickedAM)
	newDS, err := db.datasDB.Commit(ctx, branchDS, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta:    meta,
		Parents: []hash.Hash{intoHash},
	})
	if err != nil {
		return nil, fmt.Errorf("commitCherryPick: committing: %w", err)
	}

	newHash, ok := newDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("commitCherryPick: no head after cherry-pick commit")
	}

	db.setAM(ctx, branch, pickedAM)
	if branch == defaultBranch {
		if err := db.persistAM(ctx, defaultBranch, pickedAM); err != nil {
			return nil, fmt.Errorf("commitCherryPick: updating working set: %w", err)
		}
	}

	cpAuthor := pickMeta.Author.Name + " <" + pickMeta.Author.Email + ">"
	cpCommitter := cpAuthor
	if meta.Committer.Name != "" && meta.Committer.Name != meta.Author.Name {
		cpCommitter = meta.Committer.Name + " <" + meta.Committer.Email + ">"
	}
	return &backends.CherryPickResult{
		CommitID:           newHash.String(),
		Message:            commitMsg,
		Author:             cpAuthor,
		Timestamp:          pickMeta.UserTimestampMillis(),
		Committer:          cpCommitter,
		CommitterTimestamp: time.Now().UnixMilli(),
	}, nil
}

// DumboDBLog implements backends.VersioningBackend.
// It returns the commit history reachable from the starting commit (HEAD or
// params.From) in reverse topological order  -- higher commits first, with ties
// broken by timestamp (newer first). Both parents of merge commits are walked,
// so feature-branch commits reachable only via parent2 are included.
// If params.Limit <= 0 the default limit of 20 applies; a limit of 0 is
// handled as "empty list" in the handler and never reaches this function.
// Each CommitInfo is annotated with Refs when its commitId matches one or more
// branch heads (git --decorate style). The connection branch (ConnBranch) gets
// two entries: "HEAD" and the bare branch name; all other branch heads get only
// their bare branch name.
func (b *Backend) DumboDBLog(ctx context.Context, params *backends.LogParams) (*backends.LogResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBLog: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBLog: database %q does not exist", params.DBName))
	}

	db.mu.RLock()
	defer db.mu.RUnlock()

	limit := int(params.Limit)
	if limit <= 0 {
		limit = 20
	}

	// Resolve the seed frontier. params.From is a set of commit hashes (the
	// frontier returned as Next on a previous page); empty means start at the
	// connection branch HEAD. Seeds are deduped so an overlapping frontier (a
	// commit and one of its ancestors) does not double-seed the iterator.
	var seeds []hash.Hash
	seedSeen := make(map[string]bool)
	addSeed := func(h hash.Hash) {
		if s := h.String(); !seedSeen[s] {
			seedSeen[s] = true
			seeds = append(seeds, h)
		}
	}
	switch {
	case params.All:
		// Seed with every branch HEAD so the walk spans the whole commit DAG
		// (git log --all, branches only). Tags are excluded.
		dsMap, dsErr := db.datasDB.Datasets(ctx)
		if dsErr != nil {
			return nil, fmt.Errorf("DumboDBLog: listing branches for all: %w", dsErr)
		}
		// A failed iteration must be surfaced: silently swallowing it could seed
		// only some branch heads and return an incomplete log.
		if iterErr := dsMap.IterAll(ctx, func(datasetID string, headAddr hash.Hash) error {
			if strings.HasPrefix(datasetID, "refs/heads/") {
				addSeed(headAddr)
			}
			return nil
		}); iterErr != nil {
			return nil, fmt.Errorf("DumboDBLog: iterating branches for all: %w", iterErr)
		}
		if len(seeds) == 0 {
			// No branch refs found; fall back to the connection branch HEAD.
			h, rErr := resolveRootishToCommitHash(ctx, db, params.Branch)
			if rErr != nil {
				return nil, fmt.Errorf("DumboDBLog: resolving branch %q: %w", params.Branch, rErr)
			}
			addSeed(h)
		}

	case len(params.From) > 0:
		for _, from := range params.From {
			h, ok := hash.MaybeParse(from)
			if !ok {
				return nil, fmt.Errorf("DumboDBLog: invalid from hash %q", from)
			}
			// Validate that the from hash resolves to an actual commit so the
			// caller gets a clear error rather than an empty result.
			if _, loadErr := datas.LoadCommitAddr(ctx, db.vs, h); loadErr != nil {
				if loadErr == datas.ErrCommitNotFound {
					return nil, fmt.Errorf("DumboDBLog: commit not found: %q", from)
				}
				return nil, fmt.Errorf("DumboDBLog: loading commit %q: %w", h, loadErr)
			}
			addSeed(h)
		}

	default:
		h, rErr := resolveRootishToCommitHash(ctx, db, params.Branch)
		if rErr != nil {
			return nil, fmt.Errorf("DumboDBLog: resolving branch %q: %w", params.Branch, rErr)
		}
		addSeed(h)
	}

	// discovered tracks every commit ever pushed onto the iterator's queue:
	// the seeds plus the parents of every commit we examine. The frontier
	// returned as Next is discovered minus the commits we examined.
	discovered := make(map[string]bool, len(seeds))
	for _, h := range seeds {
		discovered[h.String()] = true
	}
	examined := make(map[string]bool)

	// Build per-collection filter docs once. Each is evaluated per walked commit
	// against its parent1 diff (touched semantics): the walk includes only
	// commits that touched a matching document, and Stat/Patch output is scoped
	// to those documents.
	idFilters, err := buildLogFilterDocs(params.Filters)
	if err != nil {
		return nil, fmt.Errorf("DumboDBLog: building filters: %w", err)
	}

	// Build a map from commit hash string -> ref labels by iterating over all
	// branch datasets.  The connection branch (ConnBranch) gets "HEAD -> <name>",
	// every other branch gets its bare name.
	refsForCommit := make(map[string][]string)
	dsMap, dsErr := db.datasDB.Datasets(ctx)
	if dsErr == nil {
		connBranch := params.ConnBranch
		_ = dsMap.IterAll(ctx, func(datasetID string, headAddr hash.Hash) error {
			const prefix = "refs/heads/"
			if !strings.HasPrefix(datasetID, prefix) {
				return nil
			}
			branchName := datasetID[len(prefix):]
			commitStr := headAddr.String()
			if branchName == connBranch {
				// Connection branch gets HEAD decoration as two separate entries, prepended so they sort first.
				refsForCommit[commitStr] = append([]string{"HEAD", branchName}, refsForCommit[commitStr]...)
			} else {
				refsForCommit[commitStr] = append(refsForCommit[commitStr], branchName)
			}
			return nil
		})
	}
	// Non-fatal: if Datasets() fails we simply omit all ref annotations.

	// Use commitgraph's topological-order iterator so both parents of a merge
	// commit are walked. The resolver uses the same ValueStore we write to, so
	// it sees every commit DumboDB has created.
	resolver := &doltHashResolver{vs: db.vs, ns: db.ns}
	itr, err := commitgraph.GetTopologicalOrderIterator(ctx, resolver, seeds, nil)
	if err != nil {
		return nil, fmt.Errorf("DumboDBLog: building topological iterator: %w", err)
	}

	var commits []backends.CommitInfo
	for len(commits) < limit {
		ci, iterErr := itr.Next(ctx)
		if iterErr == io.EOF {
			break
		}
		if iterErr != nil {
			return nil, fmt.Errorf("DumboDBLog: walking commits: %w", iterErr)
		}
		if ci.IsGhost {
			continue
		}

		// Record this commit as examined and push its parents onto the
		// discovered set, mirroring the iterator's queue. The frontier (Next)
		// is discovered minus examined -- non-matching commits below are still
		// examined so the frontier advances past them.
		examined[ci.Hash.String()] = true
		for _, p := range ci.Parents {
			discovered[p.String()] = true
		}

		// Apply the _id filter. A non-matching commit is examined but not
		// returned; the loop keeps walking because len(commits) only counts
		// matches, so Limit counts matching commits.
		if len(idFilters) > 0 {
			matched, fErr := commitTouchesFilter(ctx, db, ci, idFilters)
			if fErr != nil {
				return nil, fmt.Errorf("DumboDBLog: filtering commit %q: %w", ci.Hash.String(), fErr)
			}
			if !matched {
				continue
			}
		}

		author := ci.Meta.Author.Name + " <" + ci.Meta.Author.Email + ">"
		committer := ci.Meta.Committer.Name + " <" + ci.Meta.Committer.Email + ">"
		if ci.Meta.Committer.Name == "" {
			committer = author
		}
		committerTS := int64(ci.Meta.TimestampMillis())
		if committerTS == 0 {
			committerTS = ci.Meta.UserTimestampMillis()
		}

		info := backends.CommitInfo{
			CommitID:           ci.Hash.String(),
			Author:             author,
			Message:            ci.Meta.Description,
			Timestamp:          ci.Meta.UserTimestampMillis(),
			Committer:          committer,
			CommitterTimestamp: committerTS,
			Refs:               refsForCommit[ci.Hash.String()],
		}
		if len(ci.Parents) >= 1 {
			info.Parent1 = ci.Parents[0].String()
		}
		if len(ci.Parents) >= 2 {
			info.Parent2 = ci.Parents[1].String()
		}

		// When stat or patch is requested, diff this commit against its first parent.
		if params.Stat || params.Patch {
			commitAM, amErr := amFromCommitHash(ctx, db, ci.Hash.String())
			if amErr != nil {
				return nil, fmt.Errorf("DumboDBLog: loading AM for commit %q: %w", ci.Hash.String(), amErr)
			}
			var parentAM prolly.AddressMap
			if len(ci.Parents) > 0 {
				parentAM, amErr = amFromCommitHash(ctx, db, ci.Parents[0].String())
				if amErr != nil {
					return nil, fmt.Errorf("DumboDBLog: loading parent AM for commit %q: %w", ci.Hash.String(), amErr)
				}
			} else {
				parentAM, amErr = prolly.NewEmptyAddressMap(db.ns)
				if amErr != nil {
					return nil, fmt.Errorf("DumboDBLog: creating empty AM: %w", amErr)
				}
			}

			if len(idFilters) > 0 {
				names, nameErr := unionCollectionNames(ctx, db.cs, parentAM, commitAM)
				if nameErr != nil {
					return nil, fmt.Errorf("DumboDBLog: collecting names for commit %q: %w", ci.Hash.String(), nameErr)
				}
				for _, name := range names {
					filter, ok := idFilters[name]
					if !ok {
						continue
					}
					aHash, _ := parentAM.Get(ctx, name)
					bHash, _ := commitAM.Get(ctx, name)
					if aHash == bHash {
						continue
					}
					aMap, _ := collectionMapFromAM(ctx, db, parentAM, name)
					bMap, _ := collectionMapFromAM(ctx, db, commitAM, name)
					addedDocs, removedDocs, modifiedDocs, sErr := scopedCollectionDiff(ctx, db.ns, aMap, bMap, filter)
					if sErr != nil {
						return nil, fmt.Errorf("DumboDBLog: scoped diff for %q in commit %q: %w", name, ci.Hash.String(), sErr)
					}
					if len(addedDocs)+len(removedDocs)+len(modifiedDocs) == 0 {
						continue
					}
					status := statusOf(aHash, bHash)
					if params.Stat {
						info.Stat = append(info.Stat, backends.TableStatus{
							Name: name, Status: status,
							Added: len(addedDocs), Modified: len(modifiedDocs), Deleted: len(removedDocs),
						})
					}
					if params.Patch {
						info.Diff = append(info.Diff, backends.CollectionDiff{
							Name: name, Status: status,
							Added: addedDocs, Removed: removedDocs, Modified: modifiedDocs,
						})
					}
				}
			} else if cErr := eachCollectionChange(ctx, db, parentAM, commitAM, func(c collectionChange) error {
				if params.Patch {
					addedDocs, removedDocs, modifiedDocs, dErr := diffCollectionMaps(ctx, db.ns, c.AMap, c.BMap)
					if dErr != nil {
						return dErr
					}
					if len(addedDocs) == 0 && len(removedDocs) == 0 && len(modifiedDocs) == 0 && !c.surfacesWithoutDocChange() {
						return nil
					}
					if params.Stat {
						info.Stat = append(info.Stat, tableStatusFrom(c, len(addedDocs), len(modifiedDocs), len(removedDocs)))
					}
					info.Diff = append(info.Diff, collectionDiffFrom(c, addedDocs, removedDocs, modifiedDocs))
					return nil
				}
				added, modified, deleted, cntErr := countCollectionMapDiffs(ctx, c.AMap, c.BMap)
				if cntErr != nil {
					return cntErr
				}
				if added == 0 && modified == 0 && deleted == 0 && !c.surfacesWithoutDocChange() {
					return nil
				}
				info.Stat = append(info.Stat, tableStatusFrom(c, added, modified, deleted))
				return nil
			}); cErr != nil {
				return nil, fmt.Errorf("DumboDBLog: diffing commit %q: %w", ci.Hash.String(), cErr)
			}

			if len(idFilters) == 0 {
				viewChanges, vErr := diffViewEntries(ctx, db.cs, db.ns, parentAM, commitAM)
				if vErr != nil {
					return nil, fmt.Errorf("DumboDBLog: view diffs for commit %q: %w", ci.Hash.String(), vErr)
				}
				for _, vc := range viewChanges {
					if params.Stat {
						info.ViewStat = append(info.ViewStat, backends.ViewStatus{Name: vc.Name, Status: vc.Status})
					}
					if params.Patch {
						info.ViewDiff = append(info.ViewDiff, vc)
					}
				}
			}
		}

		commits = append(commits, info)
	}

	// The frontier for the next page is every discovered commit we did not
	// examine. Ghost entries (e.g. parents missing from a shallow store) are
	// dropped so the caller does not re-seed a commit that yields nothing.
	var next []string
	for h := range discovered {
		if examined[h] {
			continue
		}
		ph, ok := hash.MaybeParse(h)
		if ok {
			if ci, rErr := resolver.ResolveCommitHash(ctx, ph); rErr == nil && ci.IsGhost {
				continue
			}
		}
		next = append(next, h)
	}
	sort.Strings(next)

	return &backends.LogResult{Commits: commits, Next: next}, nil
}

// inclusiveAncestorSet returns the set of commit hashes reachable from h, including
// h itself. It reuses Dolt's precomputed parents closure (datas.NewParentsClosure +
// prolly.CommitClosure.AsHashSet), the same machinery backing doltdb.Commit.GetCommitClosure.
func inclusiveAncestorSet(ctx context.Context, db *dbState, h hash.Hash) (hash.HashSet, error) {
	commit, err := datas.LoadCommitAddr(ctx, db.vs, h)
	if err != nil {
		return nil, fmt.Errorf("loading commit %q: %w", h, err)
	}
	sv, ok := commit.NomsValue().(dolttypes.SerialMessage)
	if !ok {
		return nil, fmt.Errorf("commit %q lacks a commit closure", h)
	}
	closure, err := datas.NewParentsClosure(ctx, commit, sv, db.vs, db.ns)
	if err != nil {
		return nil, fmt.Errorf("reading commit closure for %q: %w", h, err)
	}
	ancestors, err := closure.AsHashSet(ctx)
	if err != nil {
		return nil, fmt.Errorf("materializing commit closure for %q: %w", h, err)
	}
	ancestors.Insert(h)
	return ancestors, nil
}

// DumboDBBranchStatus implements backends.VersioningBackend.
//
// It ports dolt_branch_status: for each target refspec it counts commits reachable
// from the target but not the base (ahead) and the reverse (behind), using the
// commit ancestor sets. Refspecs are resolved with resolveRootishToCommitHash.
func (b *Backend) DumboDBBranchStatus(ctx context.Context, params *backends.BranchStatusParams) (*backends.BranchStatusResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBBranchStatus: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBBranchStatus: database %q does not exist", params.DBName))
	}

	// No db.mu: reads only immutable, content-addressed history; the chunk store
	// serializes ref and closure reads internally.
	baseHash, err := resolveRootishToCommitHash(ctx, db, params.Base)
	if err != nil {
		return nil, fmt.Errorf("DumboDBBranchStatus: resolving base %q: %w", params.Base, err)
	}

	result := &backends.BranchStatusResult{
		BaseTarget: params.Base,
		BaseHash:   baseHash.String(),
	}
	if len(params.Targets) == 0 {
		return result, nil
	}

	baseAncestors, err := inclusiveAncestorSet(ctx, db, baseHash)
	if err != nil {
		return nil, fmt.Errorf("DumboDBBranchStatus: base %q: %w", params.Base, err)
	}

	for _, target := range params.Targets {
		targetHash, tErr := resolveRootishToCommitHash(ctx, db, target)
		if tErr != nil {
			return nil, fmt.Errorf("DumboDBBranchStatus: resolving target %q: %w", target, tErr)
		}

		entry := backends.BranchStatusEntry{Target: target, Hash: targetHash.String()}
		if targetHash.Equal(baseHash) {
			result.Entries = append(result.Entries, entry)
			continue
		}

		targetAncestors, aErr := inclusiveAncestorSet(ctx, db, targetHash)
		if aErr != nil {
			return nil, fmt.Errorf("DumboDBBranchStatus: target %q: %w", target, aErr)
		}

		shared := 0
		for h := range targetAncestors {
			if baseAncestors.Has(h) {
				shared++
			}
		}
		entry.CommitsAhead = int32(len(targetAncestors) - shared)
		entry.CommitsBehind = int32(len(baseAncestors) - shared)
		result.Entries = append(result.Entries, entry)
	}

	return result, nil
}

// DumboDBStatus implements backends.VersioningBackend.
//
// It returns the list of collections with uncommitted changes on the working set,
// comparing the working set AM (state.branchAMs[defaultBranch]) against the HEAD committed AM.
// Each TableStatus entry carries one of "added", "modified", or "deleted".
func (b *Backend) DumboDBStatus(ctx context.Context, params *backends.VersioningStatusParams) (*backends.VersioningStatusResult, error) {
	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBStatus: opening db %q: %w", params.DBName, err)
	}

	if state == nil {
		return &backends.VersioningStatusResult{Branch: params.Branch, Tables: []backends.TableStatus{}}, nil
	}

	// Write lock: getOrInitBranchAM may initialize the branch AM on first access.
	state.mu.Lock()
	defer state.mu.Unlock()

	branch := params.Branch
	if branch == "" {
		branch = defaultBranch
	}

	headAM, err := headRootAMForBranch(ctx, state, branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBStatus: reading HEAD AM for branch %q: %w", branch, err)
	}

	workingAM, err := state.getOrInitBranchAM(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBStatus: reading working AM for branch %q: %w", branch, err)
	}

	var tables []backends.TableStatus

	err = eachCollectionChange(ctx, state, headAM, workingAM, func(c collectionChange) error {
		addedCount, modifiedCount, deletedCount, countErr := countCollectionMapDiffs(ctx, c.AMap, c.BMap)
		if countErr != nil {
			return fmt.Errorf("counting diffs for %q: %w", c.Name, countErr)
		}
		if addedCount == 0 && modifiedCount == 0 && deletedCount == 0 && !c.surfacesWithoutDocChange() {
			return nil
		}
		tables = append(tables, tableStatusFrom(c, addedCount, modifiedCount, deletedCount))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("DumboDBStatus: %w", err)
	}

	if tables == nil {
		tables = []backends.TableStatus{}
	}

	result := &backends.VersioningStatusResult{Branch: params.Branch, Tables: tables}

	viewChanges, err := diffViewEntries(ctx, state.cs, state.ns, headAM, workingAM)
	if err != nil {
		return nil, fmt.Errorf("DumboDBStatus: diffing views for db %q: %w", params.DBName, err)
	}
	for _, vc := range viewChanges {
		result.Views = append(result.Views, backends.ViewStatus{Name: vc.Name, Status: vc.Status})
	}

	if ms := state.mergeState; ms != nil {
		switch {
		case ms.isRebase:
			result.MergeOp = "rebase"
		case ms.isCherryPick:
			result.MergeOp = "cherry-pick"
		case ms.isRevert:
			result.MergeOp = "revert"
		default:
			result.MergeOp = "merge"
		}
		result.Conflicts = ms.summaries()
		if result.Conflicts == nil {
			result.Conflicts = []backends.ConflictSummary{}
		}
	}

	// commitId is only set when the working tree is identical to the checked-out
	// commit: no uncommitted changes (collections or views) AND no
	// merge/cherry-pick/rebase/revert in progress.
	if len(tables) == 0 && len(result.Views) == 0 && result.MergeOp == "" {
		if h, hErr := resolveRootishToCommitHash(ctx, state, branch); hErr == nil {
			result.CommitID = h.String()
		}
	}

	return result, nil
}

// DumboDBReset implements backends.VersioningBackend.
//
// Soft reset (Hard=false): moves HEAD to the target commit; staged root is updated to match
// the target commit's rootValue; the working tree (db.branchAMs[defaultBranch]) is left unchanged so that any
// uncommitted changes survive.
//
// Hard reset (Hard=true): moves HEAD to the target commit and resets both the working tree
// and the staged root to the target commit's rootValue, discarding all uncommitted changes.
//
// CommitID accepts any rootish expression: a 32-char commit hash, a branch or tag name,
// or a relative ancestor expression (e.g. "main~2"). HEAD/HEAD~N forms are rewritten by
// the handler to "<branch>"/"<branch>~N" before they reach the backend.
func (b *Backend) DumboDBReset(ctx context.Context, params *backends.ResetParams) (*backends.ResetResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBReset: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBReset: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	branch := params.Branch
	if branch == "" {
		branch = defaultBranch
	}
	branchDataset := "refs/heads/" + branch

	commitID := params.CommitID
	if commitID == "" {
		branchDS, dsErr := db.datasDB.GetDataset(ctx, branchDataset)
		if dsErr != nil {
			return nil, fmt.Errorf("DumboDBReset: resolving branch %q for db %q: %w", branch, params.DBName, dsErr)
		}
		headHash, ok := branchDS.MaybeHeadAddr()
		if !ok {
			return nil, fmt.Errorf("DumboDBReset: no HEAD commit for branch %q in db %q", branch, params.DBName)
		}
		commitID = headHash.String()
	}

	targetHash, err := resolveRootishToCommitHash(ctx, db, commitID)
	if err != nil {
		return nil, fmt.Errorf("DumboDBReset: %w", err)
	}
	commitID = targetHash.String()

	targetAM, err := amFromCommitHash(ctx, db, commitID)
	if err != nil {
		return nil, fmt.Errorf("DumboDBReset: resolving target commit %q: %w", commitID, err)
	}

	branchDS, dsErr := db.datasDB.GetDataset(ctx, branchDataset)
	if dsErr != nil {
		return nil, fmt.Errorf("DumboDBReset: resolving branch %q for db %q: %w", branch, params.DBName, dsErr)
	}
	if _, err := db.datasDB.SetHead(ctx, branchDS, targetHash, ""); err != nil {
		return nil, fmt.Errorf("DumboDBReset: setting HEAD for branch %q to %q: %w", branch, commitID, err)
	}

	if params.Hard {
		// Hard reset: working tree and staged root both point to the target commit.
		if err := db.persistAM(ctx, branch, targetAM); err != nil {
			return nil, fmt.Errorf("DumboDBReset: updating working set (hard) for branch %q: %w", branch, err)
		}
		db.setAM(ctx, branch, targetAM)
	} else {
		// Soft reset: keep working tree, change staged to target commit.
		stagedRV, stagedErr := amToRootValue(ctx, db, targetAM)
		if stagedErr != nil {
			return nil, fmt.Errorf("DumboDBReset (soft): building staged root: %w", stagedErr)
		}
		fallbackWS, fbErr := db.loadBranchWS(ctx, branch)
		if fbErr != nil {
			return nil, fmt.Errorf("DumboDBReset (soft): loading working set for branch %q: %w", branch, fbErr)
		}
		ws, wsErr := workingSetViaSession(ctx, sessionFromContext(ctx), fallbackWS, params.DBName, branch)
		if wsErr != nil {
			return nil, fmt.Errorf("DumboDBReset (soft): reading working set for branch %q: %w", branch, wsErr)
		}
		if err := db.updateBranchWS(ctx, branch, func(_ *doltdb.WorkingSet) (*doltdb.WorkingSet, error) {
			return ws.WithStagedRoot(stagedRV), nil
		}); err != nil {
			return nil, fmt.Errorf("DumboDBReset: updating working set (soft) for branch %q: %w", branch, err)
		}
	}

	return &backends.ResetResult{CommitID: commitID}, nil
}

// DumboDBDiff implements backends.VersioningBackend.
//
// It computes the document-level diff between two database states:
//   - If From is empty, the "a" side is HEAD (last committed state on main).
//   - If To is empty, the "b" side is the working set (latest uncommitted state).
//   - Non-empty From/To values are resolved as rootish expressions: commit hashes,
//     branch names, ancestor expressions (e.g. "main~2"), "HEAD", or "HEAD~N".
//     "HEAD" and "HEAD~N" resolve relative to params.ConnRootish (the connection's
//     own branch or snapshot, not necessarily main).
//
// Only collections with at least one change are included in the result.
// For modified documents, only the changed fields appear in a/b.
func (b *Backend) DumboDBDiff(ctx context.Context, params *backends.DiffParams) (*backends.DiffResult, error) {
	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBDiff: opening db %q: %w", params.DBName, err)
	}

	if state == nil {
		return &backends.DiffResult{Collections: []backends.CollectionDiff{}}, nil
	}

	// Write lock: getOrInitBranchAM may initialize the branch AM on first access.
	state.mu.Lock()
	defer state.mu.Unlock()

	var aAM prolly.AddressMap

	diffBranch := params.ConnRootish
	if diffBranch == "" {
		diffBranch = defaultBranch
	}

	switch {
	case params.From == "":
		// Default: HEAD committed state for the connection's branch.
		aAM, err = headRootAMForBranch(ctx, state, diffBranch)
		if err != nil {
			return nil, fmt.Errorf("DumboDBDiff: reading HEAD AM for branch %q: %w", diffBranch, err)
		}
	case params.From == "HEAD" || strings.HasPrefix(params.From, "HEAD~"):
		aAM, err = amFromHEADExpr(ctx, state, params.ConnRootish, params.From)
		if err != nil {
			return nil, fmt.Errorf("DumboDBDiff: resolving from %q: %w", params.From, err)
		}
	default:
		aAM, err = amFromRootish(ctx, state, params.From)
		if err != nil {
			return nil, fmt.Errorf("DumboDBDiff: resolving from %q: %w", params.From, err)
		}
	}

	var bAM prolly.AddressMap

	switch {
	case params.To == "":
		// Default: current working set for the connection's branch.
		bAM, err = state.getOrInitBranchAM(ctx, diffBranch)
		if err != nil {
			return nil, fmt.Errorf("DumboDBDiff: reading working AM for branch %q: %w", diffBranch, err)
		}
	case params.To == "HEAD" || strings.HasPrefix(params.To, "HEAD~"):
		bAM, err = amFromHEADExpr(ctx, state, params.ConnRootish, params.To)
		if err != nil {
			return nil, fmt.Errorf("DumboDBDiff: resolving to %q: %w", params.To, err)
		}
	default:
		bAM, err = amFromRootish(ctx, state, params.To)
		if err != nil {
			return nil, fmt.Errorf("DumboDBDiff: resolving to %q: %w", params.To, err)
		}
	}

	var diffs []backends.CollectionDiff

	err = eachCollectionChange(ctx, state, aAM, bAM, func(c collectionChange) error {
		added, removed, modified, diffErr := diffCollectionMaps(ctx, state.ns, c.AMap, c.BMap)
		if diffErr != nil {
			return fmt.Errorf("diffing collection %q: %w", c.Name, diffErr)
		}
		if len(added) == 0 && len(removed) == 0 && len(modified) == 0 && !c.surfacesWithoutDocChange() {
			return nil
		}
		diffs = append(diffs, collectionDiffFrom(c, added, removed, modified))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("DumboDBDiff: %w", err)
	}

	if diffs == nil {
		diffs = []backends.CollectionDiff{}
	}

	viewChanges, err := diffViewEntries(ctx, state.cs, state.ns, aAM, bAM)
	if err != nil {
		return nil, fmt.Errorf("DumboDBDiff: diffing views for db %q: %w", params.DBName, err)
	}

	return &backends.DiffResult{Collections: diffs, Views: viewChanges}, nil
}

// collectionMapFromAM opens the prolly.Map for a collection from an AddressMap.
// If the collection is not present in the AM, an empty map is returned.
func collectionMapFromAM(ctx context.Context, state *dbState, am prolly.AddressMap, name string) (prolly.Map, error) {
	h, err := am.Get(ctx, name)
	if err != nil {
		return prolly.Map{}, err
	}

	if h.IsEmpty() {
		return newEmptyMap(ctx, state.ns)
	}

	return openCollection(ctx, state.cs, state.ns, h)
}

// readAMFromWorkingSet reads the collections AddressMap from the working set.
// The working set always reflects the latest writes, even when those writes
// did not create a dolt commit (HEAD stays at the last explicit commit).
// Returns an error if the working set is missing or cannot be parsed.
func readAMFromWorkingSet(ctx context.Context, doltDB datas.Database, cs *nbs.GenerationalNBS, ns tree.NodeStore) (prolly.AddressMap, error) {
	wsDs, err := doltDB.GetDataset(ctx, workingSetDataset)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("getting working set dataset: %w", err)
	}
	if !wsDs.HasHead() {
		return prolly.AddressMap{}, fmt.Errorf("working set has no head")
	}
	wsHead, err := wsDs.HeadWorkingSet()
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("reading working set head: %w", err)
	}
	workingAddr := wsHead.WorkingAddr
	if workingAddr.IsEmpty() {
		return prolly.AddressMap{}, fmt.Errorf("working_root_addr is empty")
	}
	workingChunk, err := cs.Get(ctx, workingAddr)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("reading working root chunk: %w", err)
	}
	fileID := serial.GetFileID(workingChunk.Data())
	if fileID != serial.RootValueFileID {
		return prolly.AddressMap{}, fmt.Errorf("unexpected working root file ID %q", fileID)
	}
	rtvl, err := serial.TryGetRootAsRootValue(workingChunk.Data(), serial.MessagePrefixSz)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing RTVL from working set: %w", err)
	}
	amNode, _, err := tree.NodeFromBytes(rtvl.TablesBytes())
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing collections AM from working set: %w", err)
	}
	return prolly.NewAddressMap(amNode, ns)
}

// DumboDBRebase implements backends.VersioningBackend.
//
// Reapplies all commits on the current branch not reachable from Onto onto the tip
// of Onto, rewriting the branch history. Three cases:
//
//   - Abort (Abort=true): discard the in-progress rebase and restore the pre-rebase state.
//   - Continue (Continue=true): after conflict resolution, complete the current commit
//     and proceed with remaining replays.
//   - Normal rebase: find commits to replay, replay each as a 3-way merge, commit
//     each one. On conflict, pause and return *backends.DumboDBRebaseConflictError.
func (b *Backend) DumboDBRebase(ctx context.Context, params *backends.RebaseParams) (*backends.RebaseResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRebase: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBRebase: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	branch := params.Branch
	if branch == "" {
		branch = defaultBranch
	}

	// Parse committer identity for replayed commits.
	// When Committer is empty, committer defaults to the original author per commit.
	var rebaserName, rebaserEmail string
	committerStr := params.Committer
	if committerStr != "" {
		rebaserName, rebaserEmail = parseAuthorString(committerStr)
	}

	if params.Abort {
		if db.mergeState == nil || !db.mergeState.isRebase {
			return nil, fmt.Errorf("DumboDBRebase: no rebase in progress to abort")
		}
		ms := db.mergeState
		db.mergeState = nil

		// Restore the branch HEAD in doltDB to the pre-rebase commit.
		branchDS, dsErr := db.datasDB.GetDataset(ctx, "refs/heads/"+ms.intoBranch)
		if dsErr != nil {
			return nil, fmt.Errorf("DumboDBRebase: abort: resolving branch %q: %w", ms.intoBranch, dsErr)
		}
		if _, setErr := db.datasDB.SetHead(ctx, branchDS, ms.rebaseBranchHash, ""); setErr != nil {
			return nil, fmt.Errorf("DumboDBRebase: abort: resetting branch %q to pre-rebase hash: %w", ms.intoBranch, setErr)
		}
		db.setAM(ctx, ms.intoBranch, ms.premergeAM)
		_ = clearMergeState(db)

		return &backends.RebaseResult{
			CommitsReplayed: 0,
			NewTip:          ms.rebaseBranchHash.String(),
		}, nil
	}

	if params.Continue {
		if db.mergeState == nil || !db.mergeState.isRebase || db.mergeState.intoBranch != branch {
			return nil, fmt.Errorf("DumboDBRebase: no rebase in progress on branch %q", branch)
		}
		if db.mergeState.hasUnresolvedConflicts() {
			return nil, fmt.Errorf("DumboDBRebase: unresolved rebase conflicts remain")
		}
		ms := db.mergeState

		// Clear artifact maps for the paused conflict before committing.
		if clearErr := clearConflictArtifacts(ctx, db, ms); clearErr != nil {
			return nil, fmt.Errorf("DumboDBRebase: continue: clearing artifacts: %w", clearErr)
		}

		pickCommit, loadErr := datas.LoadCommitAddr(ctx, db.vs, ms.rebaseCurrentPick)
		if loadErr != nil {
			return nil, fmt.Errorf("DumboDBRebase: continue: loading paused commit %q: %w", ms.rebaseCurrentPick, loadErr)
		}
		pickMeta, metaErr := datas.GetCommitMeta(ctx, pickCommit.NomsValue())
		if metaErr != nil {
			return nil, fmt.Errorf("DumboDBRebase: continue: reading meta for paused commit: %w", metaErr)
		}

		contAM, contAMErr := b.currentWorkingAM(ctx, db, ms.intoBranch)
		if contAMErr != nil {
			return nil, fmt.Errorf("DumboDBRebase: continue: %w", contAMErr)
		}
		newTipHash, commitErr := b.commitRebasedPick(ctx, db, ms.intoBranch, ms.intoHash, ms.rebaseCurrentPick, contAM, pickMeta, rebaserName, rebaserEmail)
		if commitErr != nil {
			return nil, fmt.Errorf("DumboDBRebase: continue: committing paused commit: %w", commitErr)
		}
		ms.intoHash = newTipHash
		ms.rebaseCommitsReplayed++

		result, rebaseErr := b.replayRemainingCommits(ctx, db, ms, rebaserName, rebaserEmail)
		if rebaseErr != nil {
			return nil, rebaseErr
		}
		if result != nil {
			db.mergeState = nil
			_ = clearMergeState(db)
			return result, nil
		}
		// Another conflict was encountered; mergeState updated by replayRemainingCommits.
		summaries := db.mergeState.summaries()
		return nil, &backends.DumboDBRebaseConflictError{
			Conflicts:      summaries,
			ConflictCommit: db.mergeState.rebaseCurrentPick.String(),
		}
	}

	// Guard: reject new rebase if a merge, cherry-pick, or rebase is already in progress.
	if db.mergeState != nil {
		switch {
		case db.mergeState.isRebase:
			return nil, fmt.Errorf("DumboDBRebase: rebase already in progress on branch %q; resolve conflicts or abort first", branch)
		case db.mergeState.isCherryPick:
			return nil, fmt.Errorf("DumboDBRebase: cherry-pick in progress on branch %q; resolve conflicts or abort first", branch)
		case db.mergeState.isRevert:
			return nil, fmt.Errorf("DumboDBRebase: revert in progress on branch %q; resolve conflicts or abort first", branch)
		default:
			return nil, fmt.Errorf("DumboDBRebase: merge in progress on branch %q; resolve conflicts or abort first", branch)
		}
	}

	if params.Onto == "" {
		return nil, fmt.Errorf("DumboDBRebase: onto parameter is required")
	}

	branchDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRebase: resolving branch %q: %w", branch, err)
	}
	if !branchDS.HasHead() {
		return nil, fmt.Errorf("DumboDBRebase: branch %q has no commits", branch)
	}
	branchHead, ok := branchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("DumboDBRebase: branch %q has no head address", branch)
	}

	ontoHead, err := resolveRootishToCommitHash(ctx, db, params.Onto)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRebase: resolving onto %q: %w", params.Onto, err)
	}

	// If onto is already an ancestor of the branch HEAD, the branch already
	// sits on top of onto: there is nothing to replay (matches git, which
	// reports "Current branch is up to date" and leaves the tip untouched).
	// Without this the branch's own commits -- which are not reachable from
	// onto -- would be replayed again, duplicating them on every rebase.
	branchAncestors, err := inclusiveAncestorSet(ctx, db, branchHead)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRebase: computing branch ancestors: %w", err)
	}
	if branchAncestors.Has(ontoHead) {
		return &backends.RebaseResult{
			CommitsReplayed: 0,
			NewTip:          branchHead.String(),
		}, nil
	}

	// Find all commits on branch not reachable from ontoHead (oldest-first).
	toReplay, err := findCommitsToReplay(ctx, db, branchHead, ontoHead)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRebase: finding commits to replay: %w", err)
	}

	// If nothing to replay, the branch is already up-to-date (e.g. behind onto).
	if len(toReplay) == 0 {
		return &backends.RebaseResult{
			CommitsReplayed: 0,
			NewTip:          branchHead.String(),
		}, nil
	}

	// Capture pre-rebase AM for abort support.
	preRebaseAM, err := db.getOrInitBranchAM(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRebase: loading pre-rebase AM for branch %q: %w", branch, err)
	}

	// Move the branch HEAD to ontoHead so that subsequent Commit() calls (which require
	// the parent to be the current branch HEAD) will succeed when replaying commits.
	branchDS, err = db.datasDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRebase: re-resolving branch %q before SetHead: %w", branch, err)
	}
	if _, setErr := db.datasDB.SetHead(ctx, branchDS, ontoHead, ""); setErr != nil {
		return nil, fmt.Errorf("DumboDBRebase: moving branch %q to onto tip: %w", branch, setErr)
	}

	ms := &mergeInProgress{
		intoBranch:            branch,
		ontoBranch:            params.Onto,
		premergeAM:            preRebaseAM,
		intoHash:              ontoHead,
		isRebase:              true,
		rebaseBranchHash:      branchHead,
		rebaseRemainingHashes: toReplay,
		rebaseCommitsReplayed: 0,
	}
	db.mergeState = ms

	// Replay all commits. replayRemainingCommits consumes from ms.rebaseRemainingHashes.
	result, rebaseErr := b.replayRemainingCommits(ctx, db, ms, rebaserName, rebaserEmail)
	if rebaseErr != nil {
		return nil, rebaseErr
	}
	if result != nil {
		db.mergeState = nil
		return result, nil
	}

	// A conflict was encountered; mergeState is set.
	summaries := db.mergeState.summaries()
	return nil, &backends.DumboDBRebaseConflictError{
		Conflicts:      summaries,
		ConflictCommit: db.mergeState.rebaseCurrentPick.String(),
	}
}

// replayRemainingCommits replays commits from ms.rebaseRemainingHashes onto ms.intoHash.
// On success (all replayed), updates the branch and returns a non-nil *RebaseResult.
// On conflict, updates ms with conflict state and returns (nil, nil)  -- caller reads ms.
// On hard error, returns (nil, error).
func (b *Backend) replayRemainingCommits(ctx context.Context, db *dbState, ms *mergeInProgress, rebaserName, rebaserEmail string) (*backends.RebaseResult, error) {
	for len(ms.rebaseRemainingHashes) > 0 {
		pickHash := ms.rebaseRemainingHashes[0]
		ms.rebaseRemainingHashes = ms.rebaseRemainingHashes[1:]

		pickCommit, err := datas.LoadCommitAddr(ctx, db.vs, pickHash)
		if err != nil {
			return nil, fmt.Errorf("replayRemainingCommits: loading commit %q: %w", pickHash, err)
		}

		// Get pick's parent (the base for 3-way merge).
		parentAddrs, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, pickCommit.NomsValue().(dolttypes.SerialMessage))
		if err != nil {
			return nil, fmt.Errorf("replayRemainingCommits: reading parents for commit %q: %w", pickHash, err)
		}

		// baseAM = parent of pick (or empty for root commit).
		var baseAM prolly.AddressMap
		var pickBaseHash hash.Hash // parent commit hash; zero-value if pick has no parent
		if len(parentAddrs) == 0 {
			baseAM, err = prolly.NewEmptyAddressMap(db.ns)
			if err != nil {
				return nil, fmt.Errorf("replayRemainingCommits: creating empty base AM: %w", err)
			}
		} else {
			pickBaseHash = parentAddrs[0]
			baseAM, err = amFromCommitHash(ctx, db, parentAddrs[0].String())
			if err != nil {
				return nil, fmt.Errorf("replayRemainingCommits: loading base AM for commit %q: %w", pickHash, err)
			}
		}

		// fromAM = the pick commit's state.
		fromAM, err := amFromCommitHash(ctx, db, pickHash.String())
		if err != nil {
			return nil, fmt.Errorf("replayRemainingCommits: loading pick AM for commit %q: %w", pickHash, err)
		}

		// intoAM = the current rebased tip's state.
		intoAM, err := amFromCommitHash(ctx, db, ms.intoHash.String())
		if err != nil {
			return nil, fmt.Errorf("replayRemainingCommits: loading into AM for tip %q: %w", ms.intoHash, err)
		}

		// 3-way merge: apply pick's diff (base->from) onto the current rebased tip (into).
		// Sides are swapped so the replayed commit presents as "ours" and the
		// onto/tip as "theirs" (a rebase moves the user's commits onto a base).
		mergedAM, conflicts, viewConflicts, metaConflicts, err := mergeAddressMapsWithConflicts(ctx, db, fromAM, intoAM, baseAM, pickHash, pickBaseHash,
			fmt.Sprintf("commit '%s' (ours)", pickHash.String()), fmt.Sprintf("branch '%s' (theirs)", ms.ontoBranch))
		if err != nil {
			return nil, fmt.Errorf("replayRemainingCommits: merging commit %q: %w", pickHash, err)
		}

		if len(conflicts) > 0 || len(viewConflicts) > 0 || len(metaConflicts) > 0 {
			ms.rebaseCurrentPick = pickHash
			ms.conflicts = conflicts
			ms.viewConflicts = viewConflicts
			ms.metaConflicts = metaConflicts
			ms.resolvedAM = mergedAM
			if wsErr := persistConflictState(ctx, db, ms); wsErr != nil {
				return nil, fmt.Errorf("replayRemainingCommits: persisting conflict state: %w", wsErr)
			}
			// Signal conflict to caller via (nil, nil).
			return nil, nil
		}

		pickMeta, err := datas.GetCommitMeta(ctx, pickCommit.NomsValue())
		if err != nil {
			return nil, fmt.Errorf("replayRemainingCommits: reading meta for commit %q: %w", pickHash, err)
		}

		newTipHash, err := b.commitRebasedPick(ctx, db, ms.intoBranch, ms.intoHash, pickHash, mergedAM, pickMeta, rebaserName, rebaserEmail)
		if err != nil {
			return nil, fmt.Errorf("replayRemainingCommits: committing replayed commit %q: %w", pickHash, err)
		}

		ms.intoHash = newTipHash
		ms.rebaseCommitsReplayed++
	}

	// All commits replayed. Update working set to reflect the final state.
	finalAM, err := amFromCommitHash(ctx, db, ms.intoHash.String())
	if err != nil {
		return nil, fmt.Errorf("replayRemainingCommits: loading final AM: %w", err)
	}
	if err := db.persistAM(ctx, ms.intoBranch, finalAM); err != nil {
		return nil, fmt.Errorf("replayRemainingCommits: persisting final AM: %w", err)
	}

	return &backends.RebaseResult{
		CommitsReplayed: ms.rebaseCommitsReplayed,
		NewTip:          ms.intoHash.String(),
	}, nil
}

// commitRebasedPick creates a single-parent commit on the branch for a replayed rebase commit.
// The new commit has parent = currentTipHash and uses the original pick commit's metadata.
// Returns the new commit hash.
func (b *Backend) commitRebasedPick(
	ctx context.Context,
	db *dbState,
	branch string,
	currentTipHash hash.Hash,
	pickHash hash.Hash,
	pickedAM prolly.AddressMap,
	pickMeta *datas.CommitMeta,
	rebaserName, rebaserEmail string,
) (hash.Hash, error) {
	var meta *datas.CommitMeta
	var err error
	if rebaserName != "" {
		// Explicit committer: preserve original author, set distinct committer.
		origAuthor := datas.CommitIdent{Name: pickMeta.Author.Name, Email: pickMeta.Author.Email, Date: pickMeta.Author.Date}
		committer := datas.CommitIdent{Name: rebaserName, Email: rebaserEmail}
		meta, err = datas.NewCommitMetaWithAuthorCommitter(origAuthor, committer, pickMeta.Description)
	} else {
		// No committer override: preserve original author; committer is implicit (same identity).
		meta, err = datas.NewCommitMetaWithAuthor(pickMeta.Author.Name, pickMeta.Author.Email, pickMeta.Description, pickMeta.Author.Date.Time())
	}
	if err != nil {
		return hash.Hash{}, fmt.Errorf("commitRebasedPick: building commit meta: %w", err)
	}

	rtvlMsg := buildRootValueFlatbuffer(pickedAM)
	branchDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("commitRebasedPick: resolving branch %q: %w", branch, err)
	}
	newDS, err := db.datasDB.Commit(ctx, branchDS, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta:    meta,
		Parents: []hash.Hash{currentTipHash},
	})
	if err != nil {
		return hash.Hash{}, fmt.Errorf("commitRebasedPick: committing: %w", err)
	}

	newHash, ok := newDS.MaybeHeadAddr()
	if !ok {
		return hash.Hash{}, fmt.Errorf("commitRebasedPick: no head after commit")
	}

	// Note: in-memory AM (db.branchAMs) is updated by the caller after all commits are done.
	_ = pickHash // retained for context; not needed in commit but useful for error messages

	return newHash, nil
}

// findCommitsToReplay returns the commits on branchHead that are NOT reachable from ontoHead,
// in oldest-first (replay) order.
func findCommitsToReplay(ctx context.Context, state *dbState, branchHead, ontoHead hash.Hash) ([]hash.Hash, error) {
	// Collect all ancestors of ontoHead (inclusive) into a set.
	ontoAncestors := make(map[hash.Hash]struct{})
	queue := []hash.Hash{ontoHead}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if _, seen := ontoAncestors[h]; seen {
			continue
		}
		ontoAncestors[h] = struct{}{}
		commit, err := datas.LoadCommitAddr(ctx, state.vs, h)
		if err != nil {
			return nil, fmt.Errorf("findCommitsToReplay: loading onto ancestor %q: %w", h, err)
		}
		parents, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
		if err != nil {
			return nil, fmt.Errorf("findCommitsToReplay: reading parents of onto ancestor %q: %w", h, err)
		}
		queue = append(queue, parents...)
	}

	// Walk from branchHead, collecting commits not in ontoAncestors (newest-first).
	var toReplay []hash.Hash
	current := branchHead
	for {
		if _, inOnto := ontoAncestors[current]; inOnto {
			break
		}
		toReplay = append(toReplay, current)
		commit, err := datas.LoadCommitAddr(ctx, state.vs, current)
		if err != nil {
			return nil, fmt.Errorf("findCommitsToReplay: loading branch commit %q: %w", current, err)
		}
		parents, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
		if err != nil {
			return nil, fmt.Errorf("findCommitsToReplay: reading parents of branch commit %q: %w", current, err)
		}
		if len(parents) == 0 {
			break // root commit  -- nothing left to walk
		}
		current = parents[0]
	}

	// Reverse to oldest-first order.
	slices.Reverse(toReplay)
	return toReplay, nil
}

// DumboDBRevert applies the inverse diff introduced by the named commit onto the current
// branch's working set and creates a new commit that undoes those changes.
//
// The 3-way merge for revert uses the commit being reverted as the "base" and its parent
// as the "from" side, so the diff applied is (commit -> parent), i.e. the inverse.
func (b *Backend) DumboDBRevert(ctx context.Context, params *backends.RevertParams) (*backends.RevertResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRevert: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBRevert: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	branch := params.Branch
	if branch == "" {
		branch = defaultBranch
	}

	if params.Abort {
		if db.mergeState == nil || !db.mergeState.isRevert {
			return nil, fmt.Errorf("DumboDBRevert: no revert in progress to abort")
		}
		ms := db.mergeState
		db.mergeState = nil

		db.setAM(ctx, ms.intoBranch, ms.premergeAM)
		_ = clearMergeState(db)

		return &backends.RevertResult{Message: "revert aborted"}, nil
	}

	if params.Continue {
		if db.mergeState == nil || !db.mergeState.isRevert || db.mergeState.intoBranch != branch {
			return nil, fmt.Errorf("DumboDBRevert: no revert in progress on branch %q", branch)
		}
		if db.mergeState.hasUnresolvedConflicts() {
			return nil, fmt.Errorf("DumboDBRevert: unresolved revert conflicts remain")
		}
		ms := db.mergeState

		intoBranchDS, dsErr := db.datasDB.GetDataset(ctx, "refs/heads/"+ms.intoBranch)
		if dsErr != nil {
			return nil, fmt.Errorf("DumboDBRevert: continue: resolving branch %q: %w", ms.intoBranch, dsErr)
		}

		if clearErr := clearConflictArtifacts(ctx, db, ms); clearErr != nil {
			return nil, fmt.Errorf("DumboDBRevert: continue: clearing artifacts: %w", clearErr)
		}

		// pickHash is the commit being reverted; fromHash is the parent hash.
		contAM, contAMErr := b.currentWorkingAM(ctx, db, ms.intoBranch)
		if contAMErr != nil {
			return nil, fmt.Errorf("DumboDBRevert: continue: %w", contAMErr)
		}
		revertRes, revertErr := b.commitRevert(ctx, db, ms.intoBranch, intoBranchDS, ms.intoHash, ms.pickHash, contAM, ms.originalMsg, params.Message, params.Author)
		if revertErr != nil {
			return nil, fmt.Errorf("DumboDBRevert: continue: %w", revertErr)
		}

		db.mergeState = nil
		_ = clearMergeState(db)
		return revertRes, nil
	}

	// Guard: reject new revert if any other operation is already in progress.
	if db.mergeState != nil {
		switch {
		case db.mergeState.isRebase:
			return nil, fmt.Errorf("DumboDBRevert: rebase in progress on branch %q; resolve conflicts or abort first", branch)
		case db.mergeState.isCherryPick:
			return nil, fmt.Errorf("DumboDBRevert: cherry-pick in progress on branch %q; resolve conflicts or abort first", branch)
		case db.mergeState.isRevert:
			return nil, fmt.Errorf("DumboDBRevert: revert already in progress on branch %q; resolve conflicts or abort first", branch)
		default:
			return nil, fmt.Errorf("DumboDBRevert: merge in progress on branch %q; resolve conflicts or abort first", branch)
		}
	}

	if params.Commit == "" {
		return nil, fmt.Errorf("DumboDBRevert: commit parameter is required")
	}

	revertHash, err := resolveRootishToCommitHash(ctx, db, params.Commit)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRevert: resolving commit %q: %w", params.Commit, err)
	}

	// Load the commit to revert to read its message and find its parent.
	revertCommit, err := datas.LoadCommitAddr(ctx, db.vs, revertHash)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRevert: loading commit %q: %w", revertHash, err)
	}

	revertMeta, err := datas.GetCommitMeta(ctx, revertCommit.NomsValue())
	if err != nil {
		return nil, fmt.Errorf("DumboDBRevert: reading meta for commit %q: %w", revertHash, err)
	}
	originalMsg := revertMeta.Description

	// Get the parent hash of the commit to use as the "from" (what we're reverting to).
	parentAddrs, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, revertCommit.NomsValue().(dolttypes.SerialMessage))
	if err != nil {
		return nil, fmt.Errorf("DumboDBRevert: reading parents for commit %q: %w", revertHash, err)
	}

	// Load the commit being reverted's AM (used as the "base" in 3-way merge).
	revertAM, err := amFromCommitHash(ctx, db, revertHash.String())
	if err != nil {
		return nil, fmt.Errorf("DumboDBRevert: loading revert AM for commit %q: %w", revertHash, err)
	}

	// Load the parent AM (used as the "from" side  -- the state to revert to).
	// For a root commit with no parent, use an empty AM as the parent.
	var parentAM prolly.AddressMap
	var parentHash hash.Hash // parent commit hash; zero-value if revert target has no parent
	if len(parentAddrs) == 0 {
		parentAM, err = prolly.NewEmptyAddressMap(db.ns)
		if err != nil {
			return nil, fmt.Errorf("DumboDBRevert: creating empty parent AM: %w", err)
		}
	} else {
		parentHash = parentAddrs[0]
		parentAM, err = amFromCommitHash(ctx, db, parentAddrs[0].String())
		if err != nil {
			return nil, fmt.Errorf("DumboDBRevert: loading parent AM for commit %q: %w", revertHash, err)
		}
	}

	intoBranchDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBRevert: resolving into branch %q: %w", branch, err)
	}
	if !intoBranchDS.HasHead() {
		return nil, fmt.Errorf("DumboDBRevert: into branch %q has no commits", branch)
	}
	intoHash, ok := intoBranchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("DumboDBRevert: into branch %q has no head address", branch)
	}

	// Load the current branch's HEAD AM (the "into" side).
	intoAM, err := amFromCommitHash(ctx, db, intoHash.String())
	if err != nil {
		return nil, fmt.Errorf("DumboDBRevert: loading into AM for branch %q: %w", branch, err)
	}

	// Perform the 3-way merge to undo the commit:
	//   base = revertAM  (the commit being undone  -- what both sides had in common)
	//   from = parentAM  (the state before the commit  -- what we want "theirs" to be)
	//   into = intoAM    (current branch HEAD  -- "ours")
	// theirHash = parentHash (the "from" side commit hash)
	// baseHash  = revertHash (the "base" side commit hash)
	mergedAM, conflicts, viewConflicts, metaConflicts, err := mergeAddressMapsWithConflicts(ctx, db, intoAM, parentAM, revertAM, parentHash, revertHash,
		fmt.Sprintf("branch '%s' (ours)", branch), fmt.Sprintf("commit '%s' (theirs)", revertHash.String()))
	if err != nil {
		return nil, fmt.Errorf("DumboDBRevert: %w", err)
	}

	if len(conflicts) > 0 || len(viewConflicts) > 0 || len(metaConflicts) > 0 {
		// Capture the pre-revert AM for abort support.
		preRevertAM, err := db.getOrInitBranchAM(ctx, branch)
		if err != nil {
			return nil, fmt.Errorf("DumboDBRevert: loading pre-revert AM for branch %q: %w", branch, err)
		}

		db.mergeState = &mergeInProgress{
			intoBranch:    branch,
			premergeAM:    preRevertAM,
			intoHash:      intoHash,
			conflicts:     conflicts,
			viewConflicts: viewConflicts,
			metaConflicts: metaConflicts,
			resolvedAM:    mergedAM,
			isRevert:      true,
			pickHash:      revertHash,
			fromHash:      parentHash,
			originalMsg:   originalMsg,
		}

		if wsErr := persistConflictState(ctx, db, db.mergeState); wsErr != nil {
			db.mergeState = nil
			return nil, fmt.Errorf("DumboDBRevert: persisting conflict state: %w", wsErr)
		}

		summaries := db.mergeState.summaries()
		return nil, &backends.DumboDBRevertConflictError{Conflicts: summaries}
	}

	// Clean revert  -- commit immediately.
	return b.commitRevert(ctx, db, branch, intoBranchDS, intoHash, revertHash, mergedAM, originalMsg, params.Message, params.Author)
}

// commitRevert creates a single-parent commit on the branch applying the reverted AM.
// originalMsg is the reverted commit's message; message (if non-empty) overrides it.
// author is optional.
func (b *Backend) commitRevert(
	ctx context.Context,
	db *dbState,
	branch string,
	branchDS datas.Dataset,
	intoHash, revertHash hash.Hash,
	revertedAM prolly.AddressMap,
	originalMsg, message, author string,
) (*backends.RevertResult, error) {
	commitMsg := message
	if commitMsg == "" {
		if originalMsg != "" {
			commitMsg = "Revert \"" + originalMsg + "\"\n\nThis reverts commit " + revertHash.String() + "."
		} else {
			commitMsg = "Revert commit " + revertHash.String()
		}
	}

	commitName := "dumbodb"
	commitEmail := "dumbodb@dumbodb"
	if author != "" {
		if idx := strings.Index(author, " <"); idx >= 0 {
			commitName = author[:idx]
			commitEmail = strings.TrimSuffix(author[idx+2:], ">")
		} else {
			commitName = author
			commitEmail = author + "@dumbodb"
		}
	}

	meta, err := datas.NewCommitMeta(commitName, commitEmail, commitMsg)
	if err != nil {
		return nil, fmt.Errorf("commitRevert: building commit meta: %w", err)
	}

	rtvlMsg := buildRootValueFlatbuffer(revertedAM)
	newDS, err := db.datasDB.Commit(ctx, branchDS, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta:    meta,
		Parents: []hash.Hash{intoHash},
	})
	if err != nil {
		return nil, fmt.Errorf("commitRevert: committing: %w", err)
	}

	newHash, ok := newDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("commitRevert: no head after revert commit")
	}

	db.setAM(ctx, branch, revertedAM)
	if branch == defaultBranch {
		if err := db.persistAM(ctx, defaultBranch, revertedAM); err != nil {
			return nil, fmt.Errorf("commitRevert: updating working set: %w", err)
		}
	}

	revertAuthor := commitName + " <" + commitEmail + ">"
	return &backends.RevertResult{
		CommitID:           newHash.String(),
		Message:            commitMsg,
		Author:             revertAuthor,
		Timestamp:          time.Now().UnixMilli(),
		Committer:          revertAuthor,
		CommitterTimestamp: time.Now().UnixMilli(),
	}, nil
}

// doltHashResolver implements commitgraph.HashResolver using the low-level
// ValueStore, avoiding the doltdb package (and its transitive SQL dependency).
type doltHashResolver struct {
	vs *dolttypes.ValueStore
	ns tree.NodeStore
}

func (r *doltHashResolver) ResolveCommitHash(ctx context.Context, h hash.Hash) (*commitgraph.CommitInfo, error) {
	commit, err := datas.LoadCommitAddr(ctx, r.vs, h)
	if err != nil {
		if err == datas.ErrCommitNotFound {
			return &commitgraph.CommitInfo{Hash: h, IsGhost: true}, nil
		}
		return nil, err
	}

	meta, err := datas.GetCommitMeta(ctx, commit.NomsValue())
	if err != nil {
		return nil, err
	}

	height := commit.Height()

	sm, ok := commit.NomsValue().(dolttypes.SerialMessage)
	if !ok {
		return nil, fmt.Errorf("ResolveCommitHash: expected SerialMessage, got %T", commit.NomsValue())
	}
	parentHashes, err := dolttypes.SerialCommitParentAddrs(r.vs.Format(), sm)
	if err != nil {
		return nil, err
	}

	return &commitgraph.CommitInfo{
		Hash:    h,
		Height:  height,
		Meta:    meta,
		Parents: parentHashes,
	}, nil
}
