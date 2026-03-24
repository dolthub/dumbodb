// Copyright 2024 Dolt Inc.
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
//   - commitHash → Commit (DCMT) with rootValue = collections AddressMap bytes
//   - Collections AddressMap (ADRM) maps collection names to prolly.Map root hashes
//   - Each prolly.Map uses key=Int64(RecordID) and value=BytesAddr(BSON hash)
//
// This layout is compatible with Dolt CLI tools (dolt log, dolt fsck, etc.).
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
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dolthub/dongo/internal/backends"
)

const (
	// defaultMemTableSize is the in-memory table size for NBS.
	defaultMemTableSize = 128 * 1024 * 1024

	// mainDataset is the dataset ID used for the "heads/main" branch.
	mainDataset = "heads/main"
)

// dbState holds the open Dolt store for a single MongoDB database.
type dbState struct {
	mu     sync.RWMutex
	cs     *nbs.GenerationalNBS
	ns     tree.NodeStore
	doltDB datas.Database     // manages STRT root format; owns cs lifecycle
	ds     datas.Dataset      // current "heads/main" dataset, updated on each commit
	am     prolly.AddressMap  // current collections address map (name → prolly.Map root)
	uuids  map[string]string  // collection name → UUID string (in-memory)
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
func (b *Backend) Database(name string) (backends.Database, error) {
	return backends.DatabaseContract(&database{
		backend: b,
		name:    name,
	}), nil
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

		// Filter empty databases (no collections).
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

		ds, am, err = commitCollectionsAM(ctx, doltDB, datas.Dataset{}, am, "init")
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
			// STRT format: read the collections AM from the head commit.
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
				amValue, _, err := ds.MaybeHeadValue()
				if err != nil {
					_ = doltDB.Close()
					return nil, fmt.Errorf("dolt: reading head value for %q: %w", dbName, err)
				}

				amMsg, ok := amValue.(dolttypes.SerialMessage)
				if !ok {
					_ = doltDB.Close()
					return nil, fmt.Errorf("dolt: unexpected root value type %T for %q", amValue, dbName)
				}

				amNode, _, err := tree.NodeFromBytes([]byte(amMsg))
				if err != nil {
					_ = doltDB.Close()
					return nil, fmt.Errorf("dolt: parsing collections AM for %q: %w", dbName, err)
				}

				am, err = prolly.NewAddressMap(amNode, ns)
				if err != nil {
					_ = doltDB.Close()
					return nil, fmt.Errorf("dolt: loading collections AM for %q: %w", dbName, err)
				}
			}

		default:
			_ = doltDB.Close()
			return nil, fmt.Errorf("dolt: unexpected root chunk file ID %q for %q", fileID, dbName)
		}
	}

	db = &dbState{
		cs:     cs,
		ns:     ns,
		doltDB: doltDB,
		ds:     ds,
		am:     am,
		uuids:  make(map[string]string),
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

	newDS, err := doltDB.Commit(ctx, ds, tree.ValueFromNode(am.Node()), datas.CommitOptions{
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

	meta, err := datas.NewCommitMeta("dongo", "dongo@localhost", "migrate: ADRM to STRT")
	if err != nil {
		return fmt.Errorf("creating commit meta: %w", err)
	}

	// Create an initial commit wrapping the collections AM as its root value.
	// NewCommitForValue writes the AM value chunk and builds the commit flatbuffer,
	// but does NOT write the commit chunk itself.
	commit, err := datas.NewCommitForValue(ctx, cs, vs, ns, tree.ValueFromNode(am.Node()), datas.CommitOptions{
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
