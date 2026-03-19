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
//   - The NBS store root hash points to a prolly.AddressMap node
//   - The AddressMap maps collection names to prolly.Map root hashes
//   - Each prolly.Map uses key=Int64(RecordID) and value=BytesAddr(BSON hash)
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
)

// dbState holds the open Dolt store for a single MongoDB database.
type dbState struct {
	mu    sync.RWMutex
	cs    *nbs.GenerationalNBS
	ns    tree.NodeStore
	am    prolly.AddressMap // current root address map (collection name -> prolly Map root)
	uuids map[string]string // collection name -> UUID string (in-memory)
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
		if err := db.cs.Close(); err != nil {
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
		_ = db.cs.Close()
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

	// Load or create the root AddressMap.
	rootHash, err := cs.Root(ctx)
	if err != nil {
		_ = cs.Close()
		return nil, fmt.Errorf("dolt: reading root for %q: %w", dbName, err)
	}

	var am prolly.AddressMap

	if rootHash.IsEmpty() {
		// New database: create empty AddressMap and commit it.
		am, err = prolly.NewEmptyAddressMap(ns)
		if err != nil {
			_ = cs.Close()
			return nil, fmt.Errorf("dolt: creating empty address map for %q: %w", dbName, err)
		}

		// Write the root node to the chunk store before committing its hash
		// as the store root. Without this, Commit fails with a dangling ref
		// because the chunk for the AddressMap node hasn't been persisted yet.
		amHash, err := ns.Write(ctx, am.Node())
		if err != nil {
			_ = cs.Close()
			return nil, fmt.Errorf("dolt: writing initial address map node for %q: %w", dbName, err)
		}

		if _, err := cs.Commit(ctx, amHash, rootHash); err != nil {
			_ = cs.Close()
			return nil, fmt.Errorf("dolt: committing initial root for %q: %w", dbName, err)
		}
	} else {
		// Existing database: load the AddressMap from the root node.
		rootNode, err := ns.Read(ctx, rootHash)
		if err != nil {
			_ = cs.Close()
			return nil, fmt.Errorf("dolt: reading root node for %q: %w", dbName, err)
		}

		am, err = prolly.NewAddressMap(rootNode, ns)
		if err != nil {
			_ = cs.Close()
			return nil, fmt.Errorf("dolt: loading address map for %q: %w", dbName, err)
		}
	}

	db = &dbState{
		cs:    cs,
		ns:    ns,
		am:    am,
		uuids: make(map[string]string),
	}

	b.dbs[dbName] = db

	return db, nil
}
