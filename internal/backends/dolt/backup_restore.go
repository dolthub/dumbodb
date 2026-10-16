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
	"os"
	"path/filepath"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/datas/pull"
	"github.com/dolthub/dolt/go/store/hash"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
)

// backupRestore creates database params.As from the root stored at a backup
// URL: every branch, tag and working set comes back as it was synced. The
// store is built in a dot-prefixed staging directory and renamed in, so a
// failed restore never leaves a partial database. With Force an existing
// database of that name is dropped first (soft, so it can be undropped).
// Remotes and branch config in admin are neither restored nor cleared.
func (b *Backend) backupRestore(ctx context.Context, params *backends.BackupParams) (*backends.BackupResult, error) {
	target := params.As
	if target == "" {
		return nil, fmt.Errorf("dumboBackup restore: target database name is required")
	}
	if !backends.ValidDatabaseDirName(target) {
		return nil, fmt.Errorf("dumboBackup restore: invalid database name %q", target)
	}
	if target == "admin" || isReservedDatabase(target) {
		return nil, fmt.Errorf("dumboBackup restore: cannot restore into the %q database", target)
	}
	if params.URL == "" {
		return nil, fmt.Errorf("dumboBackup restore: url is required")
	}
	if !params.Force && b.databaseDirExists(target) {
		return nil, fmt.Errorf("dumboBackup restore: database %q already exists; use force to replace it", target)
	}

	ru, err := b.parseRemoteURL(params.URL)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup restore: %w", err)
	}
	if !isCloneableScheme(ru.Scheme) {
		return nil, fmt.Errorf("dumboBackup restore: unsupported backup scheme %q", ru.Scheme)
	}
	remoteParams, err := b.remoteDBParams(ru.Scheme)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup restore: %w", err)
	}
	backupDB, err := doltdb.LoadDoltDBWithParams(ctx, dolttypes.Format_DOLT, ru.Raw, filesys.LocalFS, remoteParams)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup restore: opening %q: %w", ru.Raw, err)
	}
	defer func() { _ = backupDB.Close() }()

	if err = requireRestorableBackup(ctx, backupDB, ru.Raw); err != nil {
		return nil, err
	}

	stageDir, err := os.MkdirTemp(b.dataDir, ".restore-*")
	if err != nil {
		return nil, fmt.Errorf("dumboBackup restore: creating staging dir: %w", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.RemoveAll(stageDir)
		}
	}()

	root, err := stageRestore(ctx, backupDB, stageDir, b.dataDir, target)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup restore: %w", err)
	}

	if err = b.installRestoredDatabase(stageDir, target, params.Force); err != nil {
		return nil, err
	}
	renamed = true

	state, err := b.getOrOpenDB(ctx, target, false)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup restore: opening restored database %q: %w", target, err)
	}
	if state == nil {
		return nil, fmt.Errorf("dumboBackup restore: restored database %q is missing", target)
	}

	return &backends.BackupResult{DB: target, URL: ru.Raw, Root: root.String()}, nil
}

func (b *Backend) databaseDirExists(name string) bool {
	b.mu.RLock()
	_, open := b.dbs[name]
	b.mu.RUnlock()
	if open {
		return true
	}
	_, err := os.Stat(filepath.Join(b.dataDir, name))
	return err == nil
}

// requireRestorableBackup refuses an empty backup and one without the main
// branch every DumboDB database must have.
func requireRestorableBackup(ctx context.Context, backupDB *doltdb.DoltDB, url string) error {
	root, err := backupDB.NomsRoot(ctx)
	if err != nil {
		return fmt.Errorf("dumboBackup restore: reading backup root: %w", err)
	}
	if root.IsEmpty() {
		return fmt.Errorf("dumboBackup restore: no backup found at %q", url)
	}
	branches, err := backupDB.GetBranches(ctx)
	if err != nil {
		return fmt.Errorf("dumboBackup restore: listing backup branches: %w", err)
	}
	for _, br := range branches {
		if br.GetPath() == defaultBranch {
			return nil
		}
	}
	return fmt.Errorf("dumboBackup restore: backup at %q has no %q branch", url, defaultBranch)
}

// stageRestore builds a database store in stageDir holding backupDB's root and
// returns that root. The store is closed on return.
func stageRestore(ctx context.Context, backupDB *doltdb.DoltDB, stageDir, dataDir, dbName string) (hash.Hash, error) {
	tableDir, err := os.MkdirTemp(dataDir, ".restore-tmp-*")
	if err != nil {
		return hash.Hash{}, fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tableDir) }()

	cs, err := openDatabaseStore(ctx, stageDir)
	if err != nil {
		return hash.Hash{}, err
	}
	staged, err := doltdb.DoltDBFromCS(cs, dbName)
	if err != nil {
		_ = cs.Close()
		return hash.Hash{}, err
	}
	defer func() { _ = staged.Close() }()

	pull.WithDiscardingStatsCh(func(statsCh chan pull.Stats) {
		err = actions.SyncRoots(ctx, backupDB, staged, tableDir, actions.SyncRootsDBRelationshipUnrelated, statsCh)
	})
	if err != nil {
		return hash.Hash{}, err
	}
	return staged.NomsRoot(ctx)
}

// installRestoredDatabase renames the staged store into place as dbName,
// first dropping an existing database of that name when force is set. b.mu is
// held throughout so no request can create dbName in between.
func (b *Backend) installRestoredDatabase(stageDir, dbName string, force bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	liveDir := filepath.Join(b.dataDir, dbName)
	_, open := b.dbs[dbName]
	_, statErr := os.Stat(liveDir)
	if open || statErr == nil {
		if !force {
			return fmt.Errorf("dumboBackup restore: database %q already exists; use force to replace it", dbName)
		}
		if err := b.dropDatabaseLocked(&backends.DropDatabaseParams{Name: dbName}); err != nil {
			return fmt.Errorf("dumboBackup restore: replacing %q: %w", dbName, err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("dumboBackup restore: checking %q: %w", dbName, statErr)
	}

	if err := os.Rename(stageDir, liveDir); err != nil {
		return fmt.Errorf("dumboBackup restore: installing %q: %w", dbName, err)
	}
	return nil
}
