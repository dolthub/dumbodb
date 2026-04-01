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
// Branch parsing: the database name may contain a __ separator (e.g. mydb__main)
// to specify the branch, but currently all data lives in a single NBS store per
// logical database name.
package dolt

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	fb "github.com/dolthub/flatbuffers/v23/go"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/types"
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
	am         prolly.AddressMap               // current collections address map (name → DTBL hash)
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
// name may be an encoded database name of the form "dbname__rootish" where
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

// splitEncodedDBName splits an encoded database name "dbname__rootish" into
// the base database name and rootish. If no __ separator is present, the
// rootish defaults to "main" (the default branch).
func splitEncodedDBName(encoded string) (dbName, rootish string) {
	if idx := strings.Index(encoded, "__"); idx >= 0 {
		return encoded[:idx], encoded[idx+2:]
	}
	return encoded, "main"
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

	meta, err := datas.NewCommitMeta("dongo", "dongo@localhost", desc)
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

	if err := updateWorkingSet(ctx, doltDB, am, am); err != nil {
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

	meta, err := datas.NewCommitMeta("dongo", "dongo@localhost", "migrate: ADRM to STRT")
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

// updateWorkingSet writes the working set with independent working and staged roots.
// This is required for `dolt status` to function — without a workingSets/heads/main
// entry, dolt panics trying to read the working set.
//
// workingAM is the latest uncommitted state; stagedAM is what has been staged for
// the next commit (typically HEAD's rootValue until an explicit stage operation).
// The RTVL chunk for workingAM must already be in the value store (written by the
// caller via vs.WriteValue). The staged RTVL is recomputed from stagedAM and its
// chunk must also be present in the store (e.g. written by a prior commit).
func updateWorkingSet(ctx context.Context, doltDB datas.Database, workingAM, stagedAM prolly.AddressMap) error {
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

	wsDs, err := doltDB.GetDataset(ctx, workingSetDataset)
	if err != nil {
		return fmt.Errorf("getting working set dataset: %w", err)
	}

	prevHash, _ := wsDs.MaybeHeadAddr()

	meta := &datas.WorkingSetMeta{
		Name:        "dongo",
		Email:       "dongo@localhost",
		Description: "dongo working set",
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

// DongoCommit implements backends.VersioningBackend.
// It commits the current working set (collections AM) with the given message,
// creating a new dolt commit on the main branch.
func (b *Backend) DongoCommit(ctx context.Context, params *backends.CommitParams) (*backends.CommitResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoCommit: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DongoCommit: database %q does not exist", params.DBName))
	}

	message := params.Message
	if message == "" {
		message = "dongo commit"
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	newDS, _, err := commitCollectionsAM(ctx, db.doltDB, db.ds, db.am, message)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoCommit: committing db %q: %w", params.DBName, err)
	}
	db.ds = newDS

	headHash, ok := newDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("dolt: DongoCommit: no head after commit for db %q", params.DBName)
	}

	return &backends.CommitResult{
		Hash:    headHash.String(),
		Branch:  "main",
		Message: message,
	}, nil
}

// DongoBranch implements backends.VersioningBackend.
func (b *Backend) DongoBranch(_ context.Context, _ *backends.BranchParams) (*backends.BranchResult, error) {
	return nil, fmt.Errorf("dolt: DongoBranch not yet implemented")
}

// DongoCurrentBranch implements backends.VersioningBackend.
// It returns the branch name encoded in the connection's database name.
// The handler has already rejected read-only rootishes before reaching here,
// so params.Branch is always a branch name.
func (b *Backend) DongoCurrentBranch(_ context.Context, params *backends.CurrentBranchParams) (*backends.CurrentBranchResult, error) {
	return &backends.CurrentBranchResult{Branch: params.Branch}, nil
}

// DongoMerge implements backends.VersioningBackend.
func (b *Backend) DongoMerge(_ context.Context, _ *backends.MergeParams) (*backends.MergeResult, error) {
	return nil, fmt.Errorf("dolt: DongoMerge not yet implemented")
}

// DongoLog implements backends.VersioningBackend.
// It returns the commit history for the given branch, walking HEAD backwards
// through the parent1 chain up to the specified limit (default 20).
// If params.From is set, traversal starts from that commit hash instead of HEAD.
func (b *Backend) DongoLog(ctx context.Context, params *backends.LogParams) (*backends.LogResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoLog: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DongoLog: database %q does not exist", params.DBName))
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
			return nil, fmt.Errorf("dolt: DongoLog: invalid from hash %q", params.From)
		}
	} else {
		var ok bool
		startHash, ok = db.ds.MaybeHeadAddr()
		if !ok {
			return &backends.LogResult{}, nil
		}
	}

	// Walk the parent chain.
	var commits []backends.CommitInfo
	currentHash := startHash
	checkFrom := params.From != ""

	for len(commits) < limit {
		commit, loadErr := datas.LoadCommitAddr(ctx, db.vs, currentHash)
		if loadErr != nil {
			if loadErr == datas.ErrCommitNotFound {
				if checkFrom {
					return nil, fmt.Errorf("dolt: DongoLog: commit not found: %q", params.From)
				}
				break
			}
			return nil, fmt.Errorf("dolt: DongoLog: loading commit %q: %w", currentHash, loadErr)
		}
		// The from hash was successfully resolved on the first iteration.
		checkFrom = false

		meta, err := datas.GetCommitMeta(ctx, commit.NomsValue())
		if err != nil {
			return nil, fmt.Errorf("dolt: DongoLog: reading meta for %q: %w", currentHash, err)
		}

		parentAddrs, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
		if err != nil {
			return nil, fmt.Errorf("dolt: DongoLog: reading parents for %q: %w", currentHash, err)
		}

		info := backends.CommitInfo{
			Hash:      currentHash.String(),
			Author:    meta.Name,
			Message:   meta.Description,
			Timestamp: int64(meta.Timestamp),
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

// DongoStatus implements backends.VersioningBackend.
//
// It returns the list of collections with uncommitted changes on the working set,
// comparing the working set AM (state.am) against the HEAD committed AM.
// Each TableStatus entry carries one of "added", "modified", or "deleted".
func (b *Backend) DongoStatus(ctx context.Context, params *backends.VersioningStatusParams) (*backends.VersioningStatusResult, error) {
	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoStatus: opening db %q: %w", params.DBName, err)
	}

	if state == nil {
		return &backends.VersioningStatusResult{Branch: params.Branch, Tables: []backends.TableStatus{}}, nil
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	headAM, err := state.headRootAM(ctx)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoStatus: reading HEAD AM for db %q: %w", params.DBName, err)
	}

	workingAM := state.am

	names, err := unionCollectionNames(ctx, headAM, workingAM)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoStatus: collecting collection names for db %q: %w", params.DBName, err)
	}

	var tables []backends.TableStatus

	for _, name := range names {
		headHash, headErr := headAM.Get(ctx, name)
		if headErr != nil {
			return nil, fmt.Errorf("dolt: DongoStatus: reading HEAD hash for %q: %w", name, headErr)
		}

		workingHash, workingErr := workingAM.Get(ctx, name)
		if workingErr != nil {
			return nil, fmt.Errorf("dolt: DongoStatus: reading working hash for %q: %w", name, workingErr)
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

// DongoReset implements backends.VersioningBackend.
//
// Soft reset (Hard=false): moves HEAD to the target commit; staged root is updated to match
// the target commit's rootValue; the working tree (db.am) is left unchanged so that any
// uncommitted changes survive.
//
// Hard reset (Hard=true): moves HEAD to the target commit and resets both the working tree
// and the staged root to the target commit's rootValue, discarding all uncommitted changes.
func (b *Backend) DongoReset(ctx context.Context, params *backends.ResetParams) (*backends.ResetResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoReset: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DongoReset: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// Parse and validate the target commit hash.
	targetHash, ok := hash.MaybeParse(params.Hash)
	if !ok {
		return nil, fmt.Errorf("dolt: DongoReset: invalid commit hash %q", params.Hash)
	}

	// Load the AM from the target commit.
	targetAM, err := amFromCommitHash(ctx, db, params.Hash)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoReset: resolving target commit %q: %w", params.Hash, err)
	}

	// Move HEAD to the target commit without touching the working set.
	newDS, err := db.doltDB.SetHead(ctx, db.ds, targetHash, "")
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoReset: setting HEAD to %q: %w", params.Hash, err)
	}
	db.ds = newDS

	if params.Hard {
		// Hard reset: working tree and staged root both point to the target commit.
		if err := updateWorkingSet(ctx, db.doltDB, targetAM, targetAM); err != nil {
			return nil, fmt.Errorf("dolt: DongoReset: updating working set (hard): %w", err)
		}
		db.am = targetAM
	} else {
		// Soft reset: keep the working tree as-is; staged root = target commit.
		if err := updateWorkingSet(ctx, db.doltDB, db.am, targetAM); err != nil {
			return nil, fmt.Errorf("dolt: DongoReset: updating working set (soft): %w", err)
		}
	}

	return &backends.ResetResult{Hash: params.Hash}, nil
}

// DongoDiff implements backends.VersioningBackend.
//
// It computes the document-level diff between two database states:
//   - If From is empty, the "a" side is HEAD (the last committed state).
//   - If To is empty, the "b" side is the working set (latest uncommitted state).
//   - Non-empty From/To values are interpreted as dolt commit hashes.
//
// Only collections with at least one change are included in the result.
// For modified documents, only the changed fields appear in a/b.
func (b *Backend) DongoDiff(ctx context.Context, params *backends.DiffParams) (*backends.DiffResult, error) {
	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoDiff: opening db %q: %w", params.DBName, err)
	}

	if state == nil {
		return &backends.DiffResult{Collections: []backends.CollectionDiff{}}, nil
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	// Resolve the "a" (from) side.
	var aAM prolly.AddressMap

	if params.From == "" {
		// Default: HEAD committed state.
		aAM, err = state.headRootAM(ctx)
		if err != nil {
			return nil, fmt.Errorf("dolt: DongoDiff: reading HEAD AM for db %q: %w", params.DBName, err)
		}
	} else {
		aAM, err = amFromCommitHash(ctx, state, params.From)
		if err != nil {
			return nil, fmt.Errorf("dolt: DongoDiff: resolving from hash %q: %w", params.From, err)
		}
	}

	// Resolve the "b" (to) side.
	var bAM prolly.AddressMap

	if params.To == "" {
		// Default: current working set (may include uncommitted writes).
		bAM = state.am
	} else {
		bAM, err = amFromCommitHash(ctx, state, params.To)
		if err != nil {
			return nil, fmt.Errorf("dolt: DongoDiff: resolving to hash %q: %w", params.To, err)
		}
	}

	// Enumerate all collection names present in either side.
	names, err := unionCollectionNames(ctx, aAM, bAM)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoDiff: collecting collection names for db %q: %w", params.DBName, err)
	}

	var diffs []backends.CollectionDiff

	for _, name := range names {
		// Load or substitute an empty map for each side.
		aMap, mapErr := collectionMapFromAM(ctx, state, aAM, name)
		if mapErr != nil {
			return nil, fmt.Errorf("dolt: DongoDiff: opening a-side map for %q.%q: %w", params.DBName, name, mapErr)
		}

		bMap, mapErr := collectionMapFromAM(ctx, state, bAM, name)
		if mapErr != nil {
			return nil, fmt.Errorf("dolt: DongoDiff: opening b-side map for %q.%q: %w", params.DBName, name, mapErr)
		}

		added, removed, modified, diffErr := diffCollectionMaps(ctx, state.ns, aMap, bMap)
		if diffErr != nil {
			return nil, fmt.Errorf("dolt: DongoDiff: diffing collection %q in db %q: %w", name, params.DBName, diffErr)
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
