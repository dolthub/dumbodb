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
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/datas/pull"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// backupsCollection is the admin-database collection that stores backup
// definitions, one document per backup keyed by "<db>.<backup>".
const backupsCollection = "system.backups"

// DumboDBBackup manages Dolt-style backups of one database. Backup
// definitions live in admin.system.backups, parallel to admin.system.remotes.
// The admin database itself can never be backed up or restored: it holds every
// user's SCRAM keys.
func (b *Backend) DumboDBBackup(ctx context.Context, params *backends.BackupParams) (*backends.BackupResult, error) {
	if params.Action == "restore" {
		return b.backupRestore(ctx, params)
	}

	if err := b.validateBackupSourceDB(params.DBName); err != nil {
		return nil, err
	}

	coll, err := b.adminCollection(backupsCollection)
	if err != nil {
		return nil, err
	}

	switch params.Action {
	case "add":
		return b.backupAdd(ctx, coll, params)
	case "list":
		return b.backupList(ctx, coll, params)
	case "remove":
		return b.backupRemove(ctx, coll, params)
	case "sync", "sync-url":
		return b.backupSync(ctx, coll, params)
	default:
		return nil, fmt.Errorf("dumboBackup: unknown action %q (expected add, list, remove, sync, sync-url, or restore)", params.Action)
	}
}

func (b *Backend) validateBackupSourceDB(dbName string) error {
	if dbName == "" {
		return fmt.Errorf("dumboBackup: database name is required")
	}
	if !backends.ValidDatabaseDirName(dbName) {
		return fmt.Errorf("dumboBackup: invalid database name %q", dbName)
	}
	if dbName == "admin" || isReservedDatabase(dbName) {
		return fmt.Errorf("dumboBackup: the %q database cannot be backed up", dbName)
	}
	return nil
}

func (b *Backend) adminCollection(name string) (backends.Collection, error) {
	adminDB, err := b.Database("admin")
	if err != nil {
		return nil, err
	}
	return adminDB.Collection(name)
}

func (b *Backend) backupAdd(ctx context.Context, coll backends.Collection, params *backends.BackupParams) (*backends.BackupResult, error) {
	if err := validateConfigName("dumboBackup", "backup", params.Name); err != nil {
		return nil, err
	}

	if _, err := os.Stat(filepath.Join(b.dataDir, params.DBName)); err != nil {
		return nil, fmt.Errorf("dumboBackup add: database %q does not exist", params.DBName)
	}

	ru, err := b.parseRemoteURL(params.URL)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup add: %w", err)
	}
	if !isCloneableScheme(ru.Scheme) {
		return nil, fmt.Errorf("dumboBackup add: unsupported backup scheme %q", ru.Scheme)
	}

	b.configURLMu.Lock()
	defer b.configURLMu.Unlock()

	id := remoteID(params.DBName, params.Name)
	existing, err := findRemoteDoc(ctx, coll, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("dumboBackup add: backup %q already exists for database %q", params.Name, params.DBName)
	}

	if err = b.checkURLConflict(ctx, "dumboBackup add", ru.Raw, "", remotesCollection, backupsCollection); err != nil {
		return nil, err
	}

	doc := must.NotFail(types.NewDocument(
		"_id", id,
		"name", params.Name,
		"db", params.DBName,
		"url", ru.Raw,
	))
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		return nil, err
	}

	return &backends.BackupResult{Backups: []backends.BackupInfo{{Name: params.Name, URL: ru.Raw}}}, nil
}

// checkURLConflict refuses a URL already used by a config in any of
// collNames, other than a backup of ownerDB. A backup sync replaces the
// destination's root, so a backup URL shared with a remote or with another
// database's backup would destroy one of them.
func (b *Backend) checkURLConflict(ctx context.Context, command, url, ownerDB string, collNames ...string) error {
	for _, collName := range collNames {
		docs, err := b.configDocsWithURL(ctx, collName, url)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			if db, _ := doc.Get("db"); collName == backupsCollection && ownerDB != "" && db == ownerDB {
				continue
			}
			id, _ := doc.Get("_id")
			return fmt.Errorf("%s: url %q is already used by %s %q", command, url, configKind(collName), id)
		}
	}
	return nil
}

// configDocsWithURL returns the admin config docs in collName whose stored url
// equals url exactly. Stored urls are parseRemoteURL output, so callers must
// pass that same normalized form.
func (b *Backend) configDocsWithURL(ctx context.Context, collName, url string) ([]*types.Document, error) {
	coll, err := b.adminCollection(collName)
	if err != nil {
		return nil, err
	}
	docs, err := queryConfigDocs(ctx, coll, nil)
	if err != nil {
		return nil, err
	}
	var out []*types.Document
	for _, doc := range docs {
		if u, _ := doc.Get("url"); u == url {
			out = append(out, doc)
		}
	}
	return out, nil
}

func configKind(collName string) string {
	if collName == backupsCollection {
		return "backup"
	}
	return "remote"
}

func (b *Backend) backupList(ctx context.Context, coll backends.Collection, params *backends.BackupParams) (*backends.BackupResult, error) {
	docs, err := queryConfigDocs(ctx, coll, must.NotFail(types.NewDocument("db", params.DBName)))
	if err != nil {
		return nil, err
	}

	out := []backends.BackupInfo{}
	for _, doc := range docs {
		// The backend filter may be a superset; re-check the db field.
		if dbv, _ := doc.Get("db"); dbv != params.DBName {
			continue
		}
		name, _ := doc.Get("name")
		url, _ := doc.Get("url")
		ns, _ := name.(string)
		us, _ := url.(string)
		out = append(out, backends.BackupInfo{Name: ns, URL: us})
	}

	return &backends.BackupResult{Backups: out}, nil
}

// backupRemove deletes the backup definition only; data already synced to the
// backup URL is left in place.
func (b *Backend) backupRemove(ctx context.Context, coll backends.Collection, params *backends.BackupParams) (*backends.BackupResult, error) {
	if params.Name == "" {
		return nil, fmt.Errorf("dumboBackup remove: backup name is required")
	}

	res, err := coll.DeleteAll(ctx, &backends.DeleteAllParams{
		IDs: []any{remoteID(params.DBName, params.Name)},
	})
	if err != nil {
		return nil, err
	}
	if res.Deleted == 0 {
		return nil, fmt.Errorf("dumboBackup remove: backup %q not found for database %q", params.Name, params.DBName)
	}

	return &backends.BackupResult{}, nil
}

// backupSync replaces the backup's root with the database's root. The root
// covers every branch, tag and working set, so uncommitted (but command-
// committed) changes are carried. Later syncs copy only missing chunks.
func (b *Backend) backupSync(ctx context.Context, coll backends.Collection, params *backends.BackupParams) (*backends.BackupResult, error) {
	url := params.URL
	if params.Action == "sync" {
		if err := validateConfigName("dumboBackup", "backup", params.Name); err != nil {
			return nil, err
		}
		doc, err := findRemoteDoc(ctx, coll, remoteID(params.DBName, params.Name))
		if err != nil {
			return nil, err
		}
		if doc == nil {
			return nil, fmt.Errorf("dumboBackup sync: backup %q not found for database %q", params.Name, params.DBName)
		}
		stored, _ := doc.Get("url")
		url, _ = stored.(string)
	} else if url == "" {
		return nil, fmt.Errorf("dumboBackup sync-url: url is required")
	}

	ru, err := b.parseRemoteURL(url)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup %s: %w", params.Action, err)
	}
	if !isCloneableScheme(ru.Scheme) {
		return nil, fmt.Errorf("dumboBackup %s: unsupported backup scheme %q", params.Action, ru.Scheme)
	}
	b.configURLMu.Lock()
	err = b.checkURLConflict(ctx, "dumboBackup "+params.Action, ru.Raw, params.DBName, remotesCollection, backupsCollection)
	b.configURLMu.Unlock()
	if err != nil {
		return nil, err
	}

	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dumboBackup %s: database %q does not exist", params.Action, params.DBName))
	}

	nbf := state.doltDB.Format()
	if remotePreparable(ru.Scheme) {
		if err = dbfactory.PrepareDB(ctx, nbf, ru.Raw, nil); err != nil {
			return nil, fmt.Errorf("dumboBackup %s: preparing %q: %w", params.Action, ru.Raw, err)
		}
	}
	remoteParams, err := b.remoteDBParams(ru.Scheme)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup %s: %w", params.Action, err)
	}
	dest, err := doltdb.LoadDoltDBWithParams(ctx, nbf, ru.Raw, filesys.LocalFS, remoteParams)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup %s: opening %q: %w", params.Action, ru.Raw, err)
	}
	defer func() { _ = dest.Close() }()

	tempDir, err := os.MkdirTemp(b.dataDir, ".backup-*")
	if err != nil {
		return nil, fmt.Errorf("dumboBackup %s: creating temp dir: %w", params.Action, err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	err = b.runAsGCSession(func() error {
		var syncErr error
		pull.WithDiscardingStatsCh(func(statsCh chan pull.Stats) {
			syncErr = actions.SyncRoots(ctx, state.doltDB, dest, tempDir, actions.SyncRootsDBRelationshipUnknown, statsCh)
		})
		return syncErr
	})
	upToDate := errors.Is(err, pull.ErrDBUpToDate)
	if err != nil && !upToDate {
		return nil, fmt.Errorf("dumboBackup %s: %w", params.Action, err)
	}

	root, err := dest.NomsRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("dumboBackup %s: reading backup root: %w", params.Action, err)
	}

	return &backends.BackupResult{URL: ru.Raw, Root: root.String(), UpToDate: upToDate}, nil
}

// gcSession is a GC roots provider owning no roots. Registering one for the
// length of an operation makes an in-flight GC wait at its pre-finalize
// safepoint until the operation ends, so chunks it reads are not swept.
type gcSession struct {
	// A zero-size struct could share its address with another allocation,
	// and the controller keys sessions by pointer.
	_ byte
}

func (*gcSession) VisitGCRoots(context.Context, string, func(hash.Hash) bool) error {
	return nil
}

// runAsGCSession runs fn as its own GC session. Unlike
// RunUnderGCSafepointKeeper, which shares one provider and panics on
// overlapping use, concurrent callers each get a fresh session.
func (b *Backend) runAsGCSession(fn func() error) error {
	if b.gcController == nil {
		return fn()
	}
	s := &gcSession{}
	if err := b.gcController.SessionCommandBegin(s); err != nil {
		return err
	}
	defer b.gcController.SessionEnd(s)
	defer b.gcController.SessionCommandEnd(s)
	return fn()
}

// queryConfigDocs returns every document in coll matching filter (nil for all).
func queryConfigDocs(ctx context.Context, coll backends.Collection, filter *types.Document) ([]*types.Document, error) {
	qr, err := coll.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return nil, err
	}
	defer qr.Iter.Close()

	var docs []*types.Document
	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
}
