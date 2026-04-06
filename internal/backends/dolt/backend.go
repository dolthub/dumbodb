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

// Package dolt provides a Dolt-backed storage backend for FerretDB.
//
// Storage hierarchy:
//   - One nbs.GenerationalNBS per MongoDB database, stored in <dataDir>/<dbName>/
//   - The NBS store root chunk is a StoreRoot flatbuffer (STRT)
//   - STRT embeds a refsAM inline: AddressMap mapping "heads/main" → commitHash
//   - commitHash → Commit (DCMT) with rootValue = RTVL (RootValue) chunk
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
// Branch parsing: the database name may contain a __d_ separator (e.g. mydb__d_main)
// to specify the branch, but currently all data lives in a single NBS store per
// logical database name.
package dolt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	fb "github.com/dolthub/flatbuffers/v23/go"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dolthub/docudolt/internal/backends"
	"github.com/dolthub/docudolt/internal/types"
)

const (
	// defaultMemTableSize is the in-memory table size for NBS.
	defaultMemTableSize = 128 * 1024 * 1024

	// mainDataset is the dataset ID used for the "refs/heads/main" branch.
	// Dolt expects the full ref path including "refs/" prefix.
	mainDataset = "refs/heads/main"

	// workingSetDataset is the dataset ID for the working set.
	// Dolt derives this from WorkingSetRefForHead: "workingSets/" + "heads/main".
	// Required for `dolt status` to work without panicking.
	workingSetDataset = "workingSets/heads/main"

	// dbBranchSep is the separator between the database name and rootish in an
	// encoded database name (e.g. "mydb__d_main", "mydb__d_feature/foo").
	// The __d_ prefix distinguishes branch-encoded names from plain database names
	// that happen to contain double underscores.
	dbBranchSep = "__d_"
)

// collectionValidator holds schema validation options for a single collection (in-memory only).
type collectionValidator struct {
	Validator        *types.Document
	ValidationLevel  string
	ValidationAction string
}

// cappedCollectionMeta holds capped collection configuration (in-memory only).
type cappedCollectionMeta struct {
	CappedSize      int64
	CappedDocuments int64
}

// viewMeta holds collection view definition (in-memory only).
type viewMeta struct {
	ViewOn   string
	Pipeline *types.Array
}

// timeSeriesMeta holds time series collection configuration (in-memory only).
type timeSeriesMeta struct {
	TimeField   string
	MetaField   string
	Granularity string
}

// dbState holds the open Dolt store for a single MongoDB database.
type dbState struct {
	mu     sync.RWMutex
	cs     *nbs.GenerationalNBS
	ns     tree.NodeStore
	vs     *dolttypes.ValueStore // value store for writing RTVL chunks without committing
	doltDB datas.Database        // manages STRT root format; owns cs lifecycle
	ds     datas.Dataset         // "heads/main" dataset; HEAD stays fixed after init
	am         prolly.AddressMap               // current collections address map (name → DTBL hash) for main
	branchAMs  map[string]prolly.AddressMap    // per-branch working-set address maps (branch name → AM)
	uuids      map[string]string               // collection name → UUID string (in-memory)
	indexes    map[string][]backends.IndexInfo // collection name → secondary indexes (in-memory)
	validators map[string]*collectionValidator // collection name → validator (in-memory)
	capped     map[string]*cappedCollectionMeta // collection name → capped config (in-memory)
	// insertionOrder tracks document _id values in insertion order for FIFO eviction in capped collections.
	insertionOrder map[string][]any
	views          map[string]*viewMeta         // collection name → view definition (in-memory)
	timeSeries     map[string]*timeSeriesMeta   // collection name → time series config (in-memory)

	// collSchemaHash is the hash of the shared DSCH (TableSchema) chunk for the
	// collection schema: _id VARBINARY NOT NULL PK, doc JSON NOT NULL.
	// Written once at DB open and reused for all DTBL construction.
	collSchemaHash hash.Hash
	// emptyIndexAM is an empty AddressMap used for the DTBL secondary_indexes field.
	emptyIndexAM prolly.AddressMap
	// mergeState is non-nil when a merge is in progress (conflicts exist that must be resolved
	// before DocuDoltCommit will succeed). Protected by mu.
	mergeState *mergeInProgress
}

// Backend implements backends.Backend using Dolt storage.
type Backend struct {
	dataDir string
	l       *slog.Logger

	mu  sync.RWMutex
	dbs map[string]*dbState // dbName -> dbState
}

// NewBackend creates a new Dolt Backend, storing data under dataDir.
func NewBackend(dataDir string, l *slog.Logger) (backends.Backend, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("dolt: creating data directory: %w", err)
	}

	b := &Backend{
		dataDir: dataDir,
		l:       l,
		dbs:     make(map[string]*dbState),
	}

	// Initialize the admin database so it always exists on disk, matching
	// MongoDB's behavior where the admin database is always present even when
	// empty. Without this, compact on a non-existent collection in admin
	// incorrectly returns "database does not exist" instead of "collection
	// does not exist".
	if _, err := b.getOrOpenDB(context.Background(), "admin", true); err != nil {
		return nil, fmt.Errorf("dolt: initializing admin database: %w", err)
	}

	return backends.BackendContract(b), nil
}

// Close implements backends.Backend.
func (b *Backend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for name, db := range b.dbs {
		db.mu.Lock()
		if err := db.doltDB.Close(); err != nil {
			b.l.Error("dolt: closing database", "db", name, "err", err)
		}
		db.mu.Unlock()
	}

	b.dbs = make(map[string]*dbState)
}

// Status implements backends.Backend.
func (b *Backend) Status(ctx context.Context, params *backends.StatusParams) (*backends.StatusResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var totalCollections int64

	for _, db := range b.dbs {
		db.mu.RLock()
		count, err := db.am.Count()
		db.mu.RUnlock()

		if err != nil {
			return nil, err
		}

		totalCollections += int64(count)
	}

	return &backends.StatusResult{
		CountCollections: totalCollections,
	}, nil
}

// Database implements backends.Backend.
//
// name may be an encoded database name of the form "dbname__d_rootish" where
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

// splitEncodedDBName splits an encoded database name "dbname__d_rootish" into
// the base database name and rootish. If no __d_ separator is present, the
// rootish defaults to "main" (the default branch).
//
// The rootish component is percent-decoded (RFC 3986 path encoding) so that
// branch names containing characters invalid in MongoDB database names (e.g. '.'
// in "v1.0", '/' in "feature/foo") can be encoded by the client as "v1%2E0" or
// "feature%2Ffoo". The handler has already validated the encoding before the
// backend is reached, so decode errors here fall back to the raw value.
//
// All-digit strings after __d_ (e.g. Unix nanosecond timestamps) are not valid
// rootish expressions and cause the whole encoded name to be treated as a plain
// database name. This prevents spurious "not found as branch or tag" errors when
// client code accidentally produces database names like "prefix__d_1775505756999075683".
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
			return encoded[:idx], candidate
		}
	}
	return encoded, "main"
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

// ListDatabases implements backends.Backend.
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

		dbName := entry.Name()

		// System databases are always included, matching MongoDB's behavior where
		// admin, config, and local always appear in listDatabases regardless of
		// whether they contain user collections.
		isSystemDB := dbName == "admin" || dbName == "config" || dbName == "local"

		if !isSystemDB {
			// Filter empty user databases (no collections).
			state, err := b.getOrOpenDB(ctx, dbName, false)
			if err != nil {
				continue
			}

			if state == nil {
				continue
			}

			state.mu.RLock()
			count, _ := state.am.Count()
			state.mu.RUnlock()

			if count == 0 {
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

// DropDatabase implements backends.Backend.
func (b *Backend) DropDatabase(ctx context.Context, params *backends.DropDatabaseParams) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	dbDir := filepath.Join(b.dataDir, params.Name)

	if _, err := os.Stat(dbDir); os.IsNotExist(err) {
		return backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: database %q does not exist", params.Name))
	}

	// Close the open store if it exists.
	if db, ok := b.dbs[params.Name]; ok {
		db.mu.Lock()
		_ = db.doltDB.Close()
		db.mu.Unlock()
		delete(b.dbs, params.Name)
	}

	if err := os.RemoveAll(dbDir); err != nil {
		return fmt.Errorf("dolt: dropping database %q: %w", params.Name, err)
	}

	return nil
}

// Describe implements prometheus.Collector.
func (b *Backend) Describe(ch chan<- *prometheus.Desc) {}

// Collect implements prometheus.Collector.
func (b *Backend) Collect(ch chan<- prometheus.Metric) {}

// getOrOpenDB returns the dbState for the given database name,
// opening/creating the NBS store if needed.
// If create is false and the directory doesn't exist, returns nil, nil.
func (b *Backend) getOrOpenDB(ctx context.Context, dbName string, create bool) (*dbState, error) {
	b.mu.RLock()
	db, ok := b.dbs[dbName]
	b.mu.RUnlock()

	if ok {
		return db, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Double-check after acquiring write lock.
	if db, ok := b.dbs[dbName]; ok {
		return db, nil
	}

	dbDir := filepath.Join(b.dataDir, dbName)

	if !create {
		if _, err := os.Stat(dbDir); os.IsNotExist(err) {
			return nil, nil
		}
	}

	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("dolt: creating db directory for %q: %w", dbName, err)
	}

	q := nbs.NewUnlimitedMemQuotaProvider()

	newGenSt, err := nbs.NewLocalJournalingStore(ctx, dolttypes.Format_DOLT.VersionString(), dbDir, q, false, nil)
	if err != nil {
		return nil, fmt.Errorf("dolt: opening newgen NBS store for %q: %w", dbName, err)
	}

	oldgenDir := filepath.Join(dbDir, "oldgen")
	if err := os.MkdirAll(oldgenDir, 0o755); err != nil {
		_ = newGenSt.Close()
		return nil, fmt.Errorf("dolt: creating oldgen directory for %q: %w", dbName, err)
	}

	oldGenSt, err := nbs.NewLocalStore(ctx, newGenSt.Version(), oldgenDir, defaultMemTableSize, q, false)
	if err != nil {
		_ = newGenSt.Close()
		return nil, fmt.Errorf("dolt: opening oldgen NBS store for %q: %w", dbName, err)
	}

	ghostGen, err := nbs.NewGhostBlockStore(dbDir)
	if err != nil {
		_ = oldGenSt.Close()
		_ = newGenSt.Close()
		return nil, fmt.Errorf("dolt: opening ghost block store for %q: %w", dbName, err)
	}

	cs := nbs.NewGenerationalCS(oldGenSt, newGenSt, ghostGen)

	ns := tree.NewNodeStore(cs)

	// Inspect the existing root format before creating the datas.Database,
	// since datas.Database panics when reading an ADRM-format root.
	rootHash, err := cs.Root(ctx)
	if err != nil {
		_ = cs.Close()
		return nil, fmt.Errorf("dolt: reading root for %q: %w", dbName, err)
	}

	// Create the value store and dolt database which manage the STRT root format.
	vs := dolttypes.NewValueStore(cs)
	doltDB := datas.NewTypesDatabase(vs, ns)

	var am prolly.AddressMap
	var ds datas.Dataset

	if rootHash.IsEmpty() {
		// New database: create empty collections AM and write the initial STRT commit.
		am, err = prolly.NewEmptyAddressMap(ns)
		if err != nil {
			_ = doltDB.Close()
			return nil, fmt.Errorf("dolt: creating empty address map for %q: %w", dbName, err)
		}

		ds, am, err = commitCollectionsAM(ctx, doltDB, datas.Dataset{}, am, "Initialize database")
		if err != nil {
			_ = doltDB.Close()
			return nil, fmt.Errorf("dolt: initial commit for %q: %w", dbName, err)
		}
	} else {
		// Existing database: detect the root chunk format.
		rootChunk, err := cs.Get(ctx, rootHash)
		if err != nil {
			_ = doltDB.Close()
			return nil, fmt.Errorf("dolt: reading root chunk for %q: %w", dbName, err)
		}

		fileID := serial.GetFileID(rootChunk.Data())

		switch fileID {
		case serial.AddressMapFileID:
			// Legacy ADRM format: the root chunk is the collections AM directly.
			// Migrate to STRT by creating an initial dolt commit.
			b.l.Info("dolt: migrating database from ADRM to STRT root format", "db", dbName)

			amNode, _, err := tree.NodeFromChunk(&rootChunk)
			if err != nil {
				_ = doltDB.Close()
				return nil, fmt.Errorf("dolt: parsing ADRM root node for %q: %w", dbName, err)
			}

			am, err = prolly.NewAddressMap(amNode, ns)
			if err != nil {
				_ = doltDB.Close()
				return nil, fmt.Errorf("dolt: loading collections AM from ADRM root for %q: %w", dbName, err)
			}

			// Build the STRT structure manually because datas.Database panics on ADRM roots.
			// We need to do this atomically: write commit + STRT, then swap the NBS root.
			if err := migrateADRMtoSTRT(ctx, cs, vs, ns, am, rootHash); err != nil {
				_ = doltDB.Close()
				return nil, fmt.Errorf("dolt: migrating ADRM root for %q: %w", dbName, err)
			}

			// Now the NBS root is STRT; read the dataset from doltDB normally.
			ds, err = doltDB.GetDataset(ctx, mainDataset)
			if err != nil {
				_ = doltDB.Close()
				return nil, fmt.Errorf("dolt: getting dataset after migration for %q: %w", dbName, err)
			}

		case serial.StoreRootFileID:
			// STRT format: read the collections AM from the head commit's rootValue.
			ds, err = doltDB.GetDataset(ctx, mainDataset)
			if err != nil {
				_ = doltDB.Close()
				return nil, fmt.Errorf("dolt: getting dataset for %q: %w", dbName, err)
			}

			if !ds.HasHead() {
				// Shouldn't happen for a valid STRT database, but recover gracefully.
				am, err = prolly.NewEmptyAddressMap(ns)
				if err != nil {
					_ = doltDB.Close()
					return nil, fmt.Errorf("dolt: creating empty address map for %q: %w", dbName, err)
				}
			} else {
				headValue, _, err := ds.MaybeHeadValue()
				if err != nil {
					_ = doltDB.Close()
					return nil, fmt.Errorf("dolt: reading head value for %q: %w", dbName, err)
				}

				headMsg, ok := headValue.(dolttypes.SerialMessage)
				if !ok {
					_ = doltDB.Close()
					return nil, fmt.Errorf("dolt: unexpected root value type %T for %q", headValue, dbName)
				}

				headFileID := serial.GetFileID([]byte(headMsg))
				switch headFileID {
				case serial.RootValueFileID:
					// RTVL format: prefer the working set — it holds the latest state
					// after writes that don't create commits (HEAD stays at last explicit
					// commit). Fall back to HEAD rootValue if the working set is missing.
					wsAM, wsErr := readAMFromWorkingSet(ctx, doltDB, cs, ns)
					if wsErr != nil {
						b.l.Warn("dolt: working set unavailable, falling back to HEAD AM", "db", dbName, "err", wsErr)
						rtvl, fallbackErr := serial.TryGetRootAsRootValue([]byte(headMsg), serial.MessagePrefixSz)
						if fallbackErr != nil {
							_ = doltDB.Close()
							return nil, fmt.Errorf("dolt: parsing RTVL for %q: %w", dbName, fallbackErr)
						}
						amNode, _, fallbackErr := tree.NodeFromBytes(rtvl.TablesBytes())
						if fallbackErr != nil {
							_ = doltDB.Close()
							return nil, fmt.Errorf("dolt: parsing collections AM from RTVL for %q: %w", dbName, fallbackErr)
						}
						wsAM, fallbackErr = prolly.NewAddressMap(amNode, ns)
						if fallbackErr != nil {
							_ = doltDB.Close()
							return nil, fmt.Errorf("dolt: loading collections AM from RTVL for %q: %w", dbName, fallbackErr)
						}
					}
					am = wsAM

				case serial.AddressMapFileID:
					// Legacy: commit rootValue is raw ADRM. Migrate to RTVL.
					b.l.Info("dolt: migrating database from ADRM-valued commit to RTVL", "db", dbName)

					amNode, _, err := tree.NodeFromBytes([]byte(headMsg))
					if err != nil {
						_ = doltDB.Close()
						return nil, fmt.Errorf("dolt: parsing ADRM from commit for %q: %w", dbName, err)
					}

					am, err = prolly.NewAddressMap(amNode, ns)
					if err != nil {
						_ = doltDB.Close()
						return nil, fmt.Errorf("dolt: loading collections AM from commit for %q: %w", dbName, err)
					}

					ds, am, err = commitCollectionsAM(ctx, doltDB, ds, am, "migrate: wrap collections AM in RTVL")
					if err != nil {
						_ = doltDB.Close()
						return nil, fmt.Errorf("dolt: RTVL migration commit for %q: %w", dbName, err)
					}

				default:
					_ = doltDB.Close()
					return nil, fmt.Errorf("dolt: unexpected head commit rootValue file ID %q for %q", headFileID, dbName)
				}
			}

		default:
			_ = doltDB.Close()
			return nil, fmt.Errorf("dolt: unexpected root chunk file ID %q for %q", fileID, dbName)
		}
	}

	db = &dbState{
		cs:             cs,
		ns:             ns,
		vs:             vs,
		doltDB:         doltDB,
		ds:             ds,
		am:             am,
		branchAMs:      make(map[string]prolly.AddressMap),
		uuids:          make(map[string]string),
		indexes:        make(map[string][]backends.IndexInfo),
		validators:     make(map[string]*collectionValidator),
		capped:         make(map[string]*cappedCollectionMeta),
		insertionOrder: make(map[string][]any),
		views:          make(map[string]*viewMeta),
		timeSeries:     make(map[string]*timeSeriesMeta),
	}

	// Initialize DTBL construction helpers: write the shared DSCH chunk once
	// and create an empty AddressMap for secondary_indexes.
	schemaMsg := buildCollectionTableSchema()
	schemaRef, err := vs.WriteValue(ctx, dolttypes.SerialMessage(schemaMsg))
	if err != nil {
		_ = doltDB.Close()
		return nil, fmt.Errorf("dolt: writing collection schema chunk: %w", err)
	}
	db.collSchemaHash = schemaRef.TargetHash()
	db.emptyIndexAM, err = prolly.NewEmptyAddressMap(ns)
	if err != nil {
		_ = doltDB.Close()
		return nil, fmt.Errorf("dolt: creating empty index address map: %w", err)
	}

	b.dbs[dbName] = db

	return db, nil
}

// commitCollectionsAM creates a new dolt commit with the given collections
// AddressMap as its root value, updating the "heads/main" dataset.
// Returns the updated dataset and the (unchanged) AM.
func commitCollectionsAM(ctx context.Context, doltDB datas.Database, ds datas.Dataset, am prolly.AddressMap, desc string) (datas.Dataset, prolly.AddressMap, error) {
	// For a new dataset, we need to get it first.
	var err error
	if ds.ID() == "" {
		ds, err = doltDB.GetDataset(ctx, mainDataset)
		if err != nil {
			return datas.Dataset{}, am, err
		}
	}

	meta, err := datas.NewCommitMeta("dolt", "dolt@localhost", desc)
	if err != nil {
		return datas.Dataset{}, am, err
	}

	// Wrap the collections AM in an RTVL flatbuffer so dolt can read it.
	rtvlMsg := buildRootValueFlatbuffer(am)
	newDS, err := doltDB.Commit(ctx, ds, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta: meta,
	})
	if err != nil {
		return datas.Dataset{}, am, err
	}

	if err := updateWorkingSet(ctx, doltDB, am, am, "main"); err != nil {
		return datas.Dataset{}, am, fmt.Errorf("updating working set: %w", err)
	}

	return newDS, am, nil
}

// commitCollectionsAMAs creates a new dolt commit with the given collections
// AddressMap as its root value, using the provided author name and timestamp.
// Returns the updated dataset and the (unchanged) AM.
func commitCollectionsAMAs(ctx context.Context, doltDB datas.Database, ds datas.Dataset, am prolly.AddressMap, desc, authorName string, ts time.Time) (datas.Dataset, prolly.AddressMap, error) {
	var err error
	if ds.ID() == "" {
		ds, err = doltDB.GetDataset(ctx, mainDataset)
		if err != nil {
			return datas.Dataset{}, am, err
		}
	}

	var name, email string
	if idx := strings.Index(authorName, " <"); idx >= 0 {
		name = authorName[:idx]
		email = strings.TrimSuffix(authorName[idx+2:], ">")
	} else {
		name = authorName
		email = authorName + "@docudolt"
	}
	meta, err := datas.NewCommitMetaWithUserTS(name, email, desc, ts)
	if err != nil {
		return datas.Dataset{}, am, err
	}

	rtvlMsg := buildRootValueFlatbuffer(am)
	newDS, err := doltDB.Commit(ctx, ds, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta: meta,
	})
	if err != nil {
		return datas.Dataset{}, am, err
	}

	if err := updateWorkingSet(ctx, doltDB, am, am, "main"); err != nil {
		return datas.Dataset{}, am, fmt.Errorf("updating working set: %w", err)
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

	meta, err := datas.NewCommitMeta("dolt", "dolt@localhost", "migrate: ADRM to STRT")
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

	// Write the commit chunk to the store.
	commitRef, err := vs.WriteValue(ctx, commit.NomsValue())
	if err != nil {
		return fmt.Errorf("writing commit: %w", err)
	}

	// Build a refsAM mapping "heads/main" → commit hash.
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

	// Build the STRT flatbuffer with the refsAM bytes inline.
	strtMsg := buildStoreRootFlatbuffer(refsAM)

	// Write the STRT chunk and atomically update the NBS root.
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

// workingSetForBranch returns the Dolt dataset ID for the working set of a branch.
func workingSetForBranch(branch string) string {
	return "workingSets/heads/" + branch
}

// updateWorkingSet writes the working set with independent working and staged roots.
// This is required for `dolt status` to function — without a workingSets/heads/<branch>
// entry, dolt panics trying to read the working set.
//
// workingAM is the latest uncommitted state; stagedAM is what has been staged for
// the next commit (typically HEAD's rootValue until an explicit stage operation).
// The RTVL chunk for workingAM must already be in the value store (written by the
// caller via vs.WriteValue). The staged RTVL is recomputed from stagedAM and its
// chunk must also be present in the store (e.g. written by a prior commit).
func updateWorkingSet(ctx context.Context, doltDB datas.Database, workingAM, stagedAM prolly.AddressMap, branch string) error {
	workingRtvlMsg := buildRootValueFlatbuffer(workingAM)
	workingRtvlRef, err := dolttypes.NewRef(dolttypes.SerialMessage(workingRtvlMsg), dolttypes.Format_DOLT)
	if err != nil {
		return fmt.Errorf("creating working RTVL ref: %w", err)
	}

	stagedRtvlMsg := buildRootValueFlatbuffer(stagedAM)
	stagedRtvlRef, err := dolttypes.NewRef(dolttypes.SerialMessage(stagedRtvlMsg), dolttypes.Format_DOLT)
	if err != nil {
		return fmt.Errorf("creating staged RTVL ref: %w", err)
	}

	wsDs, err := doltDB.GetDataset(ctx, workingSetForBranch(branch))
	if err != nil {
		return fmt.Errorf("getting working set dataset: %w", err)
	}

	prevHash, _ := wsDs.MaybeHeadAddr()

	meta := &datas.WorkingSetMeta{
		Name:        "dolt",
		Email:       "dolt@localhost",
		Description: "dolt working set",
	}

	spec := datas.WorkingSetSpec{
		Meta:        meta,
		WorkingRoot: workingRtvlRef,
		StagedRoot:  stagedRtvlRef,
	}

	_, err = doltDB.UpdateWorkingSet(ctx, wsDs, spec, prevHash)
	return err
}

// Verify that Backend implements VersioningBackend.
var _ backends.VersioningBackend = (*Backend)(nil)

// DocuDoltCommit implements backends.VersioningBackend.
// It commits the current working set (collections AM) with the given message,
// author, and timestamp, creating a new dolt commit on the specified branch.
// If params.Branch is empty it defaults to "main".
func (b *Backend) DocuDoltCommit(ctx context.Context, params *backends.CommitParams) (*backends.CommitResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCommit: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DocuDoltCommit: database %q does not exist", params.DBName))
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
		branch = "main"
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// Guard: reject docuDoltCommit during any in-progress merge or cherry-pick.
	if db.mergeState != nil && db.mergeState.intoBranch == branch {
		if db.mergeState.hasUnresolvedConflicts() {
			if db.mergeState.isCherryPick {
				return nil, fmt.Errorf("doltCommit: unresolved cherry-pick conflicts remain")
			}
			return nil, fmt.Errorf("doltCommit: unresolved merge conflicts remain")
		}
		if db.mergeState.isCherryPick {
			return nil, fmt.Errorf("doltCommit: cherry-pick in progress: use docuDoltCherryPick continue")
		}
		return nil, fmt.Errorf("doltCommit: merge in progress: use docuDoltMerge continue")
	}

	if branch == "main" {
		newDS, _, err := commitCollectionsAMAs(ctx, db.doltDB, db.ds, db.am, message, params.Author, ts)
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltCommit: committing db %q: %w", params.DBName, err)
		}
		db.ds = newDS

		headHash, ok := newDS.MaybeHeadAddr()
		if !ok {
			return nil, fmt.Errorf("dolt: DocuDoltCommit: no head after commit for db %q", params.DBName)
		}

		return &backends.CommitResult{
			CommitID:  headHash.String(),
			Branch:    branch,
			Message:   message,
			Author:    params.Author,
			Timestamp: ts.UnixMilli(),
		}, nil
	}

	// Non-main branch commit: get the branch dataset and its working AM.
	branchDS, err := db.doltDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCommit: resolving branch %q: %w", branch, err)
	}
	if !branchDS.HasHead() {
		return nil, fmt.Errorf("dolt: DocuDoltCommit: branch %q has no commits", branch)
	}

	branchAM, err := db.getOrInitBranchAM(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCommit: loading branch AM for %q: %w", branch, err)
	}

	var name, email string
	if idx := strings.Index(params.Author, " <"); idx >= 0 {
		name = params.Author[:idx]
		email = strings.TrimSuffix(params.Author[idx+2:], ">")
	} else {
		name = params.Author
		email = params.Author + "@docudolt"
	}
	meta, err := datas.NewCommitMetaWithUserTS(name, email, message, ts)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCommit: building commit meta for branch %q: %w", branch, err)
	}

	rtvlMsg := buildRootValueFlatbuffer(branchAM)
	newDS, err := db.doltDB.Commit(ctx, branchDS, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{Meta: meta})
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCommit: committing branch %q: %w", branch, err)
	}

	if err := updateWorkingSet(ctx, db.doltDB, branchAM, branchAM, branch); err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCommit: updating working set for branch %q: %w", branch, err)
	}

	// Clear the cached branch AM so the next access reloads from the new HEAD.
	delete(db.branchAMs, branch)

	headHash, ok := newDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("dolt: DocuDoltCommit: no head after commit for branch %q", branch)
	}

	return &backends.CommitResult{
		CommitID:  headHash.String(),
		Branch:    branch,
		Message:   message,
		Author:    params.Author,
		Timestamp: ts.UnixMilli(),
	}, nil
}

// DocuDoltBranch implements backends.VersioningBackend.
//
// When params.Delete is false (default), it creates a new Dolt branch named
// params.Name, starting from the HEAD commit of the source branch params.From.
//
// When params.Delete is true, it deletes the branch named params.Name:
//   - Safe delete (Force=false, -d semantics): refuses if the branch HEAD is not
//     reachable from any other branch (i.e. data would be lost).
//   - Force delete (Force=true, -D semantics): deletes unconditionally.
//
// Both branch names map to dataset IDs of the form "refs/heads/<name>".
func (b *Backend) DocuDoltBranch(ctx context.Context, params *backends.BranchParams) (*backends.BranchResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltBranch: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DocuDoltBranch: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if params.Delete {
		return docuDoltBranchDelete(ctx, db, params)
	}

	// Resolve From to a commit hash. From may be a branch name, commit hash, or
	// ancestor expression (e.g. "main~1"), so we use the general rootish resolver.
	headHash, err := resolveRootishToCommitHash(ctx, db, params.From)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltBranch: resolving source %q: %w", params.From, err)
	}

	newDatasetID := "refs/heads/" + params.Name
	newDS, err := db.doltDB.GetDataset(ctx, newDatasetID)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltBranch: getting new branch dataset %q: %w", params.Name, err)
	}
	if newDS.HasHead() {
		return nil, fmt.Errorf("dolt: DocuDoltBranch: branch %q already exists", params.Name)
	}

	if _, err = db.doltDB.SetHead(ctx, newDS, headHash, ""); err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltBranch: creating branch %q: %w", params.Name, err)
	}

	return &backends.BranchResult{Branch: params.Name}, nil
}

// docuDoltBranchDelete deletes the branch named params.Name.
// Caller must hold db.mu.Lock().
func docuDoltBranchDelete(ctx context.Context, db *dbState, params *backends.BranchParams) (*backends.BranchResult, error) {
	// Refuse to delete the current connection's branch.
	if params.Name == params.From {
		return nil, fmt.Errorf("dolt: DocuDoltBranch: cannot delete the currently checked-out branch %q", params.Name)
	}

	datasetID := "refs/heads/" + params.Name
	branchDS, err := db.doltDB.GetDataset(ctx, datasetID)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltBranch: getting branch dataset %q: %w", params.Name, err)
	}
	if !branchDS.HasHead() {
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: DocuDoltBranch: branch %q does not exist", params.Name))
	}

	branchHash, ok := branchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("dolt: DocuDoltBranch: branch %q has no HEAD commit", params.Name)
	}

	if !params.Force {
		// Safe delete: check if branchHash is reachable from any other branch.
		// "Reachable" means branchHash is an ancestor of (or equal to) another
		// branch's HEAD.  We use FindCommonAncestor(branchCommit, otherCommit)
		// and compare the result to branchHash.
		branchCommit, loadErr := datas.LoadCommitAddr(ctx, db.vs, branchHash)
		if loadErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltBranch: loading commit for branch %q: %w", params.Name, loadErr)
		}

		dsMap, dsErr := db.doltDB.Datasets(ctx)
		if dsErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltBranch: listing datasets: %w", dsErr)
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
				return nil // skip self
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
				return errFound // stop iterating early
			}
			return nil
		})
		if iterErr != nil && !errors.Is(iterErr, errFound) {
			return nil, fmt.Errorf("dolt: DocuDoltBranch: iterating datasets: %w", iterErr)
		}

		if !reachable {
			return nil, fmt.Errorf(
				"dolt: DocuDoltBranch: branch %q has unmerged commits; use -D to force delete",
				params.Name,
			)
		}
	}

	// Delete the working set for this branch if it exists (best-effort).
	wsID := workingSetForBranch(params.Name)
	wsDS, wsErr := db.doltDB.GetDataset(ctx, wsID)
	if wsErr == nil && wsDS.HasHead() {
		_, _ = db.doltDB.Delete(ctx, wsDS, "")
	}

	// Delete the branch dataset.
	if _, err = db.doltDB.Delete(ctx, branchDS, ""); err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltBranch: deleting branch %q: %w", params.Name, err)
	}

	// Clear any cached branch AM.
	delete(db.branchAMs, params.Name)

	return &backends.BranchResult{Branch: params.Name}, nil
}

// DocuDoltCurrentBranch implements backends.VersioningBackend.
// It returns the branch name encoded in the connection's database name.
// The handler has already rejected read-only rootishes before reaching here,
// so params.Branch is always a branch name.
func (b *Backend) DocuDoltCurrentBranch(_ context.Context, params *backends.CurrentBranchParams) (*backends.CurrentBranchResult, error) {
	return &backends.CurrentBranchResult{Branch: params.Branch}, nil
}

// DocuDoltMerge implements backends.VersioningBackend.
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
//     resolve conflicts via DocuDoltResolveConflict before DocuDoltCommit will succeed.
func (b *Backend) DocuDoltMerge(ctx context.Context, params *backends.MergeParams) (*backends.MergeResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DocuDoltMerge: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// Handle abort: discard in-progress merge and restore pre-merge state.
	if params.Abort {
		if db.mergeState == nil {
			return nil, fmt.Errorf("dolt: DocuDoltMerge: no merge in progress to abort")
		}
		if db.mergeState.isCherryPick {
			return nil, fmt.Errorf("dolt: DocuDoltMerge: cherry-pick in progress on branch %q; use docuDoltCherryPick abort instead", params.Into)
		}
		ms := db.mergeState
		db.mergeState = nil

		// Restore the working set to the pre-merge AM.
		if ms.intoBranch == "main" {
			db.am = ms.premergeAM
		} else {
			db.branchAMs[ms.intoBranch] = ms.premergeAM
		}

		return &backends.MergeResult{Message: "merge aborted"}, nil
	}

	// Handle continue: resume after conflict resolution and create the merge commit.
	if params.Continue {
		if db.mergeState == nil || db.mergeState.intoBranch != params.Into {
			return nil, fmt.Errorf("doltMerge: no merge in progress")
		}
		if db.mergeState.isCherryPick {
			return nil, fmt.Errorf("dolt: DocuDoltMerge: cherry-pick in progress on branch %q; use docuDoltCherryPick continue instead", params.Into)
		}
		if db.mergeState.hasUnresolvedConflicts() {
			return nil, fmt.Errorf("doltMerge: unresolved merge conflicts remain")
		}
		ms := db.mergeState

		intoBranchDS, err := db.doltDB.GetDataset(ctx, "refs/heads/"+ms.intoBranch)
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltMerge: continue: resolving branch %q: %w", ms.intoBranch, err)
		}

		mergeRes, err := b.commitMerge(ctx, db, ms.fromBranch, ms.intoBranch, intoBranchDS, ms.intoHash, ms.fromHash, ms.resolvedAM, params.Message, params.Author)
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltMerge: continue: %w", err)
		}

		db.mergeState = nil
		return mergeRes, nil
	}

	// Guard: reject new merge initiation if a merge or cherry-pick is already in progress.
	if db.mergeState != nil {
		if db.mergeState.isCherryPick {
			return nil, fmt.Errorf("dolt: DocuDoltMerge: cherry-pick in progress on branch %q; resolve conflicts or abort first", params.Into)
		}
		return nil, fmt.Errorf("dolt: DocuDoltMerge: merge already in progress on branch %q; resolve conflicts or abort first", params.Into)
	}

	// Resolve the Into branch dataset.
	intoBranchDS, err := db.doltDB.GetDataset(ctx, "refs/heads/"+params.Into)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: resolving into branch %q: %w", params.Into, err)
	}
	if !intoBranchDS.HasHead() {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: into branch %q has no commits", params.Into)
	}
	intoHash, ok := intoBranchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: into branch %q has no head address", params.Into)
	}

	// Resolve the From branch dataset.
	fromBranchDS, err := db.doltDB.GetDataset(ctx, "refs/heads/"+params.From)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: resolving from branch %q: %w", params.From, err)
	}
	if !fromBranchDS.HasHead() {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: from branch %q has no commits", params.From)
	}
	fromHash, ok := fromBranchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: from branch %q has no head address", params.From)
	}

	// Load commit objects for LCA computation.
	intoCommit, err := datas.LoadCommitAddr(ctx, db.vs, intoHash)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: loading into commit: %w", err)
	}
	fromCommit, err := datas.LoadCommitAddr(ctx, db.vs, fromHash)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: loading from commit: %w", err)
	}

	// Find the lowest common ancestor.
	baseHash, hasBase, err := datas.FindCommonAncestor(ctx, intoCommit, fromCommit, db.vs, db.vs, db.ns, db.ns)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: finding common ancestor: %w", err)
	}
	if !hasBase {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: branches %q and %q have no common ancestor", params.Into, params.From)
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
		return nil, fmt.Errorf("dolt: DocuDoltMerge: not possible to fast-forward")
	}

	// Fast-forward: Into's HEAD is an ancestor of From's HEAD.
	if baseHash == intoHash && !params.NoFF {
		newDS, ffErr := db.doltDB.SetHead(ctx, intoBranchDS, fromHash, "")
		if ffErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltMerge: fast-forward: advancing branch pointer: %w", ffErr)
		}
		if params.Into == "main" {
			db.ds = newDS
			db.am, err = amFromCommitHash(ctx, db, fromHash.String())
			if err != nil {
				return nil, fmt.Errorf("dolt: DocuDoltMerge: fast-forward: loading AM: %w", err)
			}
			if err := updateWorkingSet(ctx, db.doltDB, db.am, db.am, "main"); err != nil {
				return nil, fmt.Errorf("dolt: DocuDoltMerge: fast-forward: updating working set: %w", err)
			}
		}
		return &backends.MergeResult{
			CommitID: fromHash.String(),
			Message:  "fast-forward",
		}, nil
	}

	// True 3-way merge (or forced non-fast-forward): load AddressMaps and attempt to merge.
	intoAM, err := amFromCommitHash(ctx, db, intoHash.String())
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: loading into AM: %w", err)
	}
	fromAM, err := amFromCommitHash(ctx, db, fromHash.String())
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: loading from AM: %w", err)
	}
	baseAM, err := amFromCommitHash(ctx, db, baseHash.String())
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: loading base AM: %w", err)
	}

	mergedAM, conflicts, err := mergeAddressMapsWithConflicts(ctx, db, intoAM, fromAM, baseAM)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltMerge: %w", err)
	}

	if len(conflicts) > 0 {
		// Capture the pre-merge working set AM for abort support.
		var preMergeAM prolly.AddressMap
		if params.Into == "main" {
			preMergeAM = db.am
		} else {
			preMergeAM, err = db.getOrInitBranchAM(ctx, params.Into)
			if err != nil {
				return nil, fmt.Errorf("dolt: DocuDoltMerge: loading premerge AM for branch %q: %w", params.Into, err)
			}
		}

		db.mergeState = &mergeInProgress{
			fromBranch: params.From,
			intoBranch: params.Into,
			premergeAM: preMergeAM,
			fromHash:   fromHash,
			intoHash:   intoHash,
			conflicts:  conflicts,
			resolvedAM: mergedAM,
		}

		// Build the conflict summary for the error response.
		summaries := db.mergeState.summaries()
		return nil, &backends.MergeConflictError{Conflicts: summaries}
	}

	// Clean merge — commit immediately.
	return b.commitMerge(ctx, db, params.From, params.Into, intoBranchDS, intoHash, fromHash, mergedAM, params.Message, params.Author)
}

// commitMerge creates a merge commit on intoBranch with both branch HEADs as parents.
// Called for clean merges (no conflicts) and for continue (conflict-resolved) merges from DocuDoltMerge.
// message and author are optional; if empty, defaults are used.
func (b *Backend) commitMerge(
	ctx context.Context,
	db *dbState,
	fromBranch, intoBranch string,
	intoBranchDS datas.Dataset,
	intoHash, fromHash hash.Hash,
	mergedAM prolly.AddressMap,
	message, author string,
) (*backends.MergeResult, error) {
	mergeMessage := message
	if mergeMessage == "" {
		mergeMessage = fmt.Sprintf("Merge branch '%s' into '%s'", fromBranch, intoBranch)
	}

	commitName := "dolt"
	commitEmail := "dolt@localhost"
	if author != "" {
		if idx := strings.Index(author, " <"); idx >= 0 {
			commitName = author[:idx]
			commitEmail = strings.TrimSuffix(author[idx+2:], ">")
		} else {
			commitName = author
			commitEmail = author + "@docudolt"
		}
	}

	meta, err := datas.NewCommitMeta(commitName, commitEmail, mergeMessage)
	if err != nil {
		return nil, fmt.Errorf("dolt: commitMerge: building commit meta: %w", err)
	}

	rtvlMsg := buildRootValueFlatbuffer(mergedAM)
	newDS, err := db.doltDB.Commit(ctx, intoBranchDS, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta:    meta,
		Parents: []hash.Hash{intoHash, fromHash},
	})
	if err != nil {
		return nil, fmt.Errorf("dolt: commitMerge: committing merge: %w", err)
	}

	mergeHash, ok := newDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("dolt: commitMerge: no head after merge commit")
	}

	if intoBranch == "main" {
		db.ds = newDS
		db.am = mergedAM
		if err := updateWorkingSet(ctx, db.doltDB, mergedAM, mergedAM, "main"); err != nil {
			return nil, fmt.Errorf("dolt: commitMerge: updating working set: %w", err)
		}
	} else {
		db.branchAMs[intoBranch] = mergedAM
	}

	return &backends.MergeResult{
		CommitID: mergeHash.String(),
		Message:  mergeMessage,
	}, nil
}

// DocuDoltCherryPick implements backends.VersioningBackend.
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
//     *backends.DocuDoltCherryPickConflictError.
func (b *Backend) DocuDoltCherryPick(ctx context.Context, params *backends.CherryPickParams) (*backends.CherryPickResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DocuDoltCherryPick: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	branch := params.Branch
	if branch == "" {
		branch = "main"
	}

	// Handle abort: discard in-progress cherry-pick and restore pre-pick state.
	if params.Abort {
		if db.mergeState == nil || !db.mergeState.isCherryPick {
			return nil, fmt.Errorf("dolt: DocuDoltCherryPick: no cherry-pick in progress to abort")
		}
		ms := db.mergeState
		db.mergeState = nil

		// Restore the working set to the pre-pick AM.
		if ms.intoBranch == "main" {
			db.am = ms.premergeAM
		} else {
			db.branchAMs[ms.intoBranch] = ms.premergeAM
		}

		return &backends.CherryPickResult{Message: "cherry-pick aborted"}, nil
	}

	// Handle continue: resume after conflict resolution and create the cherry-pick commit.
	if params.Continue {
		if db.mergeState == nil || !db.mergeState.isCherryPick || db.mergeState.intoBranch != branch {
			return nil, fmt.Errorf("dolt: DocuDoltCherryPick: no cherry-pick in progress on branch %q", branch)
		}
		if db.mergeState.hasUnresolvedConflicts() {
			return nil, fmt.Errorf("dolt: DocuDoltCherryPick: unresolved cherry-pick conflicts remain")
		}
		ms := db.mergeState

		intoBranchDS, dsErr := db.doltDB.GetDataset(ctx, "refs/heads/"+ms.intoBranch)
		if dsErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltCherryPick: continue: resolving branch %q: %w", ms.intoBranch, dsErr)
		}

		pickRes, pickErr := b.commitCherryPick(ctx, db, ms.intoBranch, intoBranchDS, ms.intoHash, ms.pickHash, ms.resolvedAM, ms.originalMsg, params.Message, params.Author)
		if pickErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltCherryPick: continue: %w", pickErr)
		}

		db.mergeState = nil
		return pickRes, nil
	}

	// Guard: reject new cherry-pick if a merge or cherry-pick is already in progress.
	if db.mergeState != nil {
		if db.mergeState.isCherryPick {
			return nil, fmt.Errorf("dolt: DocuDoltCherryPick: cherry-pick already in progress on branch %q; resolve conflicts or abort first", branch)
		}
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: merge in progress on branch %q; resolve conflicts or abort first", branch)
	}

	if params.Commit == "" {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: commit parameter is required")
	}

	// Resolve the commit to cherry-pick.
	pickHash, err := resolveRootishToCommitHash(ctx, db, params.Commit)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: resolving commit %q: %w", params.Commit, err)
	}

	// Load the cherry-pick commit to read its message and find its parent.
	pickCommit, err := datas.LoadCommitAddr(ctx, db.vs, pickHash)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: loading commit %q: %w", pickHash, err)
	}

	pickMeta, err := datas.GetCommitMeta(ctx, pickCommit.NomsValue())
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: reading meta for commit %q: %w", pickHash, err)
	}
	originalMsg := pickMeta.Description

	// Get the parent hash of the commit to use as the merge base.
	parentAddrs, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, pickCommit.NomsValue().(dolttypes.SerialMessage))
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: reading parents for commit %q: %w", pickHash, err)
	}

	// Load the base AM (parent of the cherry-picked commit).
	// For a root commit with no parent, use an empty AM as the base.
	var baseAM prolly.AddressMap
	if len(parentAddrs) == 0 {
		baseAM, err = prolly.NewEmptyAddressMap(db.ns)
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltCherryPick: creating empty base AM: %w", err)
		}
	} else {
		baseAM, err = amFromCommitHash(ctx, db, parentAddrs[0].String())
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltCherryPick: loading parent AM for commit %q: %w", pickHash, err)
		}
	}

	// Load the cherry-pick commit's AM (the "from" side).
	fromAM, err := amFromCommitHash(ctx, db, pickHash.String())
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: loading pick AM for commit %q: %w", pickHash, err)
	}

	// Resolve the current branch dataset.
	intoBranchDS, err := db.doltDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: resolving into branch %q: %w", branch, err)
	}
	if !intoBranchDS.HasHead() {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: into branch %q has no commits", branch)
	}
	intoHash, ok := intoBranchDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: into branch %q has no head address", branch)
	}

	// Load the current branch's HEAD AM (the "into" side).
	intoAM, err := amFromCommitHash(ctx, db, intoHash.String())
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: loading into AM for branch %q: %w", branch, err)
	}

	// Perform the 3-way merge: apply cherry-pick diff (base→from) onto current HEAD (into).
	mergedAM, conflicts, err := mergeAddressMapsWithConflicts(ctx, db, intoAM, fromAM, baseAM)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltCherryPick: %w", err)
	}

	if len(conflicts) > 0 {
		// Capture the pre-pick AM for abort support.
		var prePickAM prolly.AddressMap
		if branch == "main" {
			prePickAM = db.am
		} else {
			prePickAM, err = db.getOrInitBranchAM(ctx, branch)
			if err != nil {
				return nil, fmt.Errorf("dolt: DocuDoltCherryPick: loading pre-pick AM for branch %q: %w", branch, err)
			}
		}

		db.mergeState = &mergeInProgress{
			intoBranch:   branch,
			premergeAM:   prePickAM,
			intoHash:     intoHash,
			conflicts:    conflicts,
			resolvedAM:   mergedAM,
			isCherryPick: true,
			pickHash:     pickHash,
			originalMsg:  originalMsg,
		}

		summaries := db.mergeState.summaries()
		return nil, &backends.DocuDoltCherryPickConflictError{Conflicts: summaries}
	}

	// Clean cherry-pick — commit immediately.
	return b.commitCherryPick(ctx, db, branch, intoBranchDS, intoHash, pickHash, mergedAM, originalMsg, params.Message, params.Author)
}

// commitCherryPick creates a single-parent commit on the branch applying the cherry-picked AM.
// originalMsg is the cherry-picked commit's message; message (if non-empty) overrides it.
// author is optional.
func (b *Backend) commitCherryPick(
	ctx context.Context,
	db *dbState,
	branch string,
	branchDS datas.Dataset,
	intoHash, pickHash hash.Hash,
	pickedAM prolly.AddressMap,
	originalMsg, message, author string,
) (*backends.CherryPickResult, error) {
	commitMsg := message
	if commitMsg == "" {
		if originalMsg != "" {
			commitMsg = originalMsg + "\n\n(cherry picked from commit " + pickHash.String() + ")"
		} else {
			commitMsg = "cherry picked from commit " + pickHash.String()
		}
	}

	commitName := "dolt"
	commitEmail := "dolt@localhost"
	if author != "" {
		if idx := strings.Index(author, " <"); idx >= 0 {
			commitName = author[:idx]
			commitEmail = strings.TrimSuffix(author[idx+2:], ">")
		} else {
			commitName = author
			commitEmail = author + "@docudolt"
		}
	}

	meta, err := datas.NewCommitMeta(commitName, commitEmail, commitMsg)
	if err != nil {
		return nil, fmt.Errorf("dolt: commitCherryPick: building commit meta: %w", err)
	}

	rtvlMsg := buildRootValueFlatbuffer(pickedAM)
	newDS, err := db.doltDB.Commit(ctx, branchDS, dolttypes.SerialMessage(rtvlMsg), datas.CommitOptions{
		Meta:    meta,
		Parents: []hash.Hash{intoHash},
	})
	if err != nil {
		return nil, fmt.Errorf("dolt: commitCherryPick: committing: %w", err)
	}

	newHash, ok := newDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("dolt: commitCherryPick: no head after cherry-pick commit")
	}

	if branch == "main" {
		db.ds = newDS
		db.am = pickedAM
		if err := updateWorkingSet(ctx, db.doltDB, pickedAM, pickedAM, "main"); err != nil {
			return nil, fmt.Errorf("dolt: commitCherryPick: updating working set: %w", err)
		}
	} else {
		db.branchAMs[branch] = pickedAM
	}

	return &backends.CherryPickResult{
		CommitID: newHash.String(),
		Message:  commitMsg,
	}, nil
}

// mergeAddressMaps performs a 3-way merge of three collections AddressMaps.
//
// For each collection name found in any of the three maps:
//   - If only From changed it (added, modified, or deleted): apply From's change.
//   - If only Into changed it: keep Into's version (already present in intoAM).
//   - If both changed it and both deleted it: keep the deletion (already absent in intoAM).
//   - If both changed it and one deleted while the other modified: return a conflict error.
//   - If both changed the same collection's documents differently: return a conflict error.
func mergeAddressMaps(ctx context.Context, state *dbState, intoAM, fromAM, baseAM prolly.AddressMap) (prolly.AddressMap, error) {
	// Collect all collection names across all three maps.
	allNames := make(map[string]struct{})
	for _, am := range []prolly.AddressMap{intoAM, fromAM, baseAM} {
		if err := am.IterAll(ctx, func(name string, _ hash.Hash) error {
			allNames[name] = struct{}{}
			return nil
		}); err != nil {
			return prolly.AddressMap{}, fmt.Errorf("iterating collections AM: %w", err)
		}
	}

	editor := intoAM.Editor()

	for name := range allNames {
		intoH, err := intoAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("reading into AM for %q: %w", name, err)
		}
		fromH, err := fromAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("reading from AM for %q: %w", name, err)
		}
		baseH, err := baseAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("reading base AM for %q: %w", name, err)
		}

		intoChanged := intoH != baseH
		fromChanged := fromH != baseH

		if !fromChanged {
			// From did not touch this collection; keep Into's version.
			continue
		}

		if !intoChanged {
			// Only From changed this collection; apply From's change to the editor.
			switch {
			case fromH.IsEmpty():
				// From deleted the collection.
				if err := editor.Delete(ctx, name); err != nil {
					return prolly.AddressMap{}, fmt.Errorf("deleting collection %q: %w", name, err)
				}
			case intoH.IsEmpty():
				// From added a collection that was not in base or into.
				if err := editor.Add(ctx, name, fromH); err != nil {
					return prolly.AddressMap{}, fmt.Errorf("adding collection %q: %w", name, err)
				}
			default:
				// From modified an existing collection.
				if err := editor.Update(ctx, name, fromH); err != nil {
					return prolly.AddressMap{}, fmt.Errorf("updating collection %q: %w", name, err)
				}
			}
			continue
		}

		// Both sides changed this collection.
		if fromH.IsEmpty() && intoH.IsEmpty() {
			// Both independently deleted the collection; result is deletion (already absent in intoAM).
			continue
		}
		if fromH.IsEmpty() || intoH.IsEmpty() {
			// One side deleted while the other modified — unresolvable conflict.
			return prolly.AddressMap{}, fmt.Errorf("conflict in collection %q: deleted on one branch and modified on the other", name)
		}

		// Both sides modified the collection; merge at the document level.
		intoMap, err := openCollection(ctx, state.cs, state.ns, intoH)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("opening into collection %q: %w", name, err)
		}
		fromMap, err := openCollection(ctx, state.cs, state.ns, fromH)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("opening from collection %q: %w", name, err)
		}
		baseMap, err := openCollection(ctx, state.cs, state.ns, baseH)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("opening base collection %q: %w", name, err)
		}

		var conflictDetected bool
		mergedMap, _, err := prolly.MergeMaps(ctx, intoMap, fromMap, baseMap, func(left, right tree.Diff) (tree.Diff, bool) {
			conflictDetected = true
			return tree.Diff{}, false
		})
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("merging collection %q documents: %w", name, err)
		}
		if conflictDetected {
			return prolly.AddressMap{}, fmt.Errorf("conflict in collection %q: same document modified differently on both branches", name)
		}

		mergedH, err := state.dtblHashForMap(ctx, mergedMap)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("writing merged collection %q: %w", name, err)
		}
		if err := editor.Update(ctx, name, mergedH); err != nil {
			return prolly.AddressMap{}, fmt.Errorf("updating merged collection %q in AM: %w", name, err)
		}
	}

	return editor.Flush(ctx)
}

// DocuDoltLog implements backends.VersioningBackend.
// It returns the commit history for the given branch, walking HEAD backwards
// through the parent1 chain up to the specified limit (default 20).
// If params.From is set, traversal starts from that commit hash instead of HEAD.
// Each CommitInfo is annotated with Refs when its commitId matches one or more
// branch heads (git --decorate style). The connection branch (ConnBranch) gets
// two entries: "HEAD" and the bare branch name; all other branch heads get only
// their bare branch name.
// TODO: tag decoration is not yet supported.
func (b *Backend) DocuDoltLog(ctx context.Context, params *backends.LogParams) (*backends.LogResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltLog: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DocuDoltLog: database %q does not exist", params.DBName))
	}

	db.mu.RLock()
	defer db.mu.RUnlock()

	limit := int(params.Limit)
	if limit <= 0 {
		limit = 20
	}

	// Determine starting commit hash.
	var startHash hash.Hash
	if params.From != "" {
		var ok bool
		startHash, ok = hash.MaybeParse(params.From)
		if !ok {
			return nil, fmt.Errorf("dolt: DocuDoltLog: invalid from hash %q", params.From)
		}
	} else {
		var ok bool
		startHash, ok = db.ds.MaybeHeadAddr()
		if !ok {
			return &backends.LogResult{}, nil
		}
	}

	// Build a map from commit hash string → ref labels by iterating over all
	// branch datasets.  The connection branch (ConnBranch) gets "HEAD -> <name>",
	// every other branch gets its bare name.
	refsForCommit := make(map[string][]string)
	dsMap, dsErr := db.doltDB.Datasets(ctx)
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

	// Walk the parent chain.
	var commits []backends.CommitInfo
	currentHash := startHash
	checkFrom := params.From != ""

	for len(commits) < limit {
		commit, loadErr := datas.LoadCommitAddr(ctx, db.vs, currentHash)
		if loadErr != nil {
			if loadErr == datas.ErrCommitNotFound {
				if checkFrom {
					return nil, fmt.Errorf("dolt: DocuDoltLog: commit not found: %q", params.From)
				}
				break
			}
			return nil, fmt.Errorf("dolt: DocuDoltLog: loading commit %q: %w", currentHash, loadErr)
		}
		// The from hash was successfully resolved on the first iteration.
		checkFrom = false

		meta, err := datas.GetCommitMeta(ctx, commit.NomsValue())
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltLog: reading meta for %q: %w", currentHash, err)
		}

		parentAddrs, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltLog: reading parents for %q: %w", currentHash, err)
		}

		info := backends.CommitInfo{
			CommitID:  currentHash.String(),
			Author:    meta.Name + " <" + meta.Email + ">",
			Message:   meta.Description,
			Timestamp: meta.UserTimestamp,
			Refs:      refsForCommit[currentHash.String()],
		}
		if len(parentAddrs) >= 1 {
			info.Parent1 = parentAddrs[0].String()
		}
		if len(parentAddrs) >= 2 {
			info.Parent2 = parentAddrs[1].String()
		}

		commits = append(commits, info)

		if len(parentAddrs) == 0 {
			break // root commit, stop walking
		}
		currentHash = parentAddrs[0]
	}

	return &backends.LogResult{Commits: commits}, nil
}

// DocuDoltStatus implements backends.VersioningBackend.
//
// It returns the list of collections with uncommitted changes on the working set,
// comparing the working set AM (state.am) against the HEAD committed AM.
// Each TableStatus entry carries one of "added", "modified", or "deleted".
func (b *Backend) DocuDoltStatus(ctx context.Context, params *backends.VersioningStatusParams) (*backends.VersioningStatusResult, error) {
	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltStatus: opening db %q: %w", params.DBName, err)
	}

	if state == nil {
		return &backends.VersioningStatusResult{Branch: params.Branch, Tables: []backends.TableStatus{}}, nil
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	headAM, err := state.headRootAM(ctx)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltStatus: reading HEAD AM for db %q: %w", params.DBName, err)
	}

	workingAM := state.am

	names, err := unionCollectionNames(ctx, headAM, workingAM)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltStatus: collecting collection names for db %q: %w", params.DBName, err)
	}

	var tables []backends.TableStatus

	for _, name := range names {
		headHash, headErr := headAM.Get(ctx, name)
		if headErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltStatus: reading HEAD hash for %q: %w", name, headErr)
		}

		workingHash, workingErr := workingAM.Get(ctx, name)
		if workingErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltStatus: reading working hash for %q: %w", name, workingErr)
		}

		var status string
		switch {
		case headHash.IsEmpty() && !workingHash.IsEmpty():
			status = "added"
		case !headHash.IsEmpty() && workingHash.IsEmpty():
			status = "deleted"
		case headHash != workingHash:
			status = "modified"
		default:
			// unchanged — skip
			continue
		}

		tables = append(tables, backends.TableStatus{Name: name, Status: status})
	}

	if tables == nil {
		tables = []backends.TableStatus{}
	}

	return &backends.VersioningStatusResult{Branch: params.Branch, Tables: tables}, nil
}

// DocuDoltReset implements backends.VersioningBackend.
//
// Soft reset (Hard=false): moves HEAD to the target commit; staged root is updated to match
// the target commit's rootValue; the working tree (db.am) is left unchanged so that any
// uncommitted changes survive.
//
// Hard reset (Hard=true): moves HEAD to the target commit and resets both the working tree
// and the staged root to the target commit's rootValue, discarding all uncommitted changes.
func (b *Backend) DocuDoltReset(ctx context.Context, params *backends.ResetParams) (*backends.ResetResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltReset: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DocuDoltReset: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// Resolve empty CommitID to the current HEAD.
	commitID := params.CommitID
	if commitID == "" {
		headHash, ok := db.ds.MaybeHeadAddr()
		if !ok {
			return nil, fmt.Errorf("dolt: DocuDoltReset: no HEAD commit for db %q", params.DBName)
		}
		commitID = headHash.String()
	}

	// Parse and validate the target commit hash.
	targetHash, ok := hash.MaybeParse(commitID)
	if !ok {
		return nil, fmt.Errorf("dolt: DocuDoltReset: invalid commit hash %q", commitID)
	}

	// Load the AM from the target commit.
	targetAM, err := amFromCommitHash(ctx, db, commitID)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltReset: resolving target commit %q: %w", commitID, err)
	}

	// Move HEAD to the target commit without touching the working set.
	newDS, err := db.doltDB.SetHead(ctx, db.ds, targetHash, "")
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltReset: setting HEAD to %q: %w", commitID, err)
	}
	db.ds = newDS

	if params.Hard {
		// Hard reset: working tree and staged root both point to the target commit.
		if err := updateWorkingSet(ctx, db.doltDB, targetAM, targetAM, "main"); err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltReset: updating working set (hard): %w", err)
		}
		db.am = targetAM
	} else {
		// Soft reset: keep the working tree as-is; staged root = target commit.
		if err := updateWorkingSet(ctx, db.doltDB, db.am, targetAM, "main"); err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltReset: updating working set (soft): %w", err)
		}
	}

	return &backends.ResetResult{CommitID: commitID}, nil
}

// DocuDoltDiff implements backends.VersioningBackend.
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
func (b *Backend) DocuDoltDiff(ctx context.Context, params *backends.DiffParams) (*backends.DiffResult, error) {
	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltDiff: opening db %q: %w", params.DBName, err)
	}

	if state == nil {
		return &backends.DiffResult{Collections: []backends.CollectionDiff{}}, nil
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	// Resolve the "a" (from) side.
	var aAM prolly.AddressMap

	switch {
	case params.From == "":
		// Default: HEAD committed state.
		aAM, err = state.headRootAM(ctx)
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltDiff: reading HEAD AM for db %q: %w", params.DBName, err)
		}
	case params.From == "HEAD" || strings.HasPrefix(params.From, "HEAD~"):
		aAM, err = amFromHEADExpr(ctx, state, params.ConnRootish, params.From)
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltDiff: resolving from %q: %w", params.From, err)
		}
	default:
		aAM, err = amFromRootish(ctx, state, params.From)
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltDiff: resolving from %q: %w", params.From, err)
		}
	}

	// Resolve the "b" (to) side.
	var bAM prolly.AddressMap

	switch {
	case params.To == "":
		// Default: current working set (may include uncommitted writes).
		bAM = state.am
	case params.To == "HEAD" || strings.HasPrefix(params.To, "HEAD~"):
		bAM, err = amFromHEADExpr(ctx, state, params.ConnRootish, params.To)
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltDiff: resolving to %q: %w", params.To, err)
		}
	default:
		bAM, err = amFromRootish(ctx, state, params.To)
		if err != nil {
			return nil, fmt.Errorf("dolt: DocuDoltDiff: resolving to %q: %w", params.To, err)
		}
	}

	// Enumerate all collection names present in either side.
	names, err := unionCollectionNames(ctx, aAM, bAM)
	if err != nil {
		return nil, fmt.Errorf("dolt: DocuDoltDiff: collecting collection names for db %q: %w", params.DBName, err)
	}

	var diffs []backends.CollectionDiff

	for _, name := range names {
		// Load or substitute an empty map for each side.
		aMap, mapErr := collectionMapFromAM(ctx, state, aAM, name)
		if mapErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltDiff: opening a-side map for %q.%q: %w", params.DBName, name, mapErr)
		}

		bMap, mapErr := collectionMapFromAM(ctx, state, bAM, name)
		if mapErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltDiff: opening b-side map for %q.%q: %w", params.DBName, name, mapErr)
		}

		added, removed, modified, diffErr := diffCollectionMaps(ctx, state.ns, aMap, bMap)
		if diffErr != nil {
			return nil, fmt.Errorf("dolt: DocuDoltDiff: diffing collection %q in db %q: %w", name, params.DBName, diffErr)
		}

		if len(added) == 0 && len(removed) == 0 && len(modified) == 0 {
			continue
		}

		diffs = append(diffs, backends.CollectionDiff{
			Name:     name,
			Added:    added,
			Removed:  removed,
			Modified: modified,
		})
	}

	if diffs == nil {
		diffs = []backends.CollectionDiff{}
	}

	return &backends.DiffResult{Collections: diffs}, nil
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
