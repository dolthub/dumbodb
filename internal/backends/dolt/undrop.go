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
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
)

// preservedDirName is the data-dir subdirectory holding soft-deleted databases.
// The leading dot keeps it out of the database namespace: MongoDB database names
// cannot contain '.', so no live database can ever collide with it, and
// ListDatabases skips dot-prefixed entries.
//
// Layout: <dataDir>/.dumbodb_dropped_databases/<dbName>/<dropId>/
// where dropId is the UnixNano instant of the drop. The two-level layout lets a
// single database be dropped repeatedly without losing earlier drops.
const preservedDirName = ".dumbodb_dropped_databases"

func (b *Backend) preservedRoot() string {
	return filepath.Join(b.dataDir, preservedDirName)
}

// preservedDest reserves and returns a fresh preserved-drops directory for a drop of
// name. The parent (<preservedRoot>/<name>) is created; the returned leaf does
// not yet exist and is the rename target for the live database directory.
//
// Caller must hold b.mu.
func (b *Backend) preservedDest(name string) (string, error) {
	parent := filepath.Join(b.preservedRoot(), name)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}

	id := time.Now().UnixNano()
	for {
		dest := filepath.Join(parent, strconv.FormatInt(id, 10))
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			return dest, nil
		} else if err != nil && !os.IsExist(err) {
			return "", err
		}
		id++
	}
}

// ListDroppedDatabases returns every preserved drop, most recent first.
func (b *Backend) ListDroppedDatabases(_ context.Context) (*backends.DroppedDatabasesResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	dropped, err := b.scanPreserved()
	if err != nil {
		return nil, err
	}
	return &backends.DroppedDatabasesResult{Databases: dropped}, nil
}

// scanPreserved walks the preserved-drops directory and returns its drops sorted by
// drop id descending (most recently dropped first). Caller must hold b.mu.
func (b *Backend) scanPreserved() ([]backends.DroppedDatabase, error) {
	root := b.preservedRoot()
	nameEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var dropped []backends.DroppedDatabase
	for _, nameEntry := range nameEntries {
		if !nameEntry.IsDir() {
			continue
		}
		name := nameEntry.Name()
		dropEntries, err := os.ReadDir(filepath.Join(root, name))
		if err != nil {
			return nil, err
		}
		for _, dropEntry := range dropEntries {
			if !dropEntry.IsDir() {
				continue
			}
			dropID := dropEntry.Name()
			nanos, perr := strconv.ParseInt(dropID, 10, 64)
			if perr != nil {
				// Not a drop id we produced; skip rather than fail the listing.
				continue
			}
			dropped = append(dropped, backends.DroppedDatabase{
				Name:              name,
				DropID:            dropID,
				DroppedAtUnixNano: nanos,
			})
		}
	}

	sort.Slice(dropped, func(i, j int) bool {
		return dropped[i].DroppedAtUnixNano > dropped[j].DroppedAtUnixNano
	})
	return dropped, nil
}

// UndropDatabase restores a soft-deleted database to a live database.
func (b *Backend) UndropDatabase(_ context.Context, params *backends.UndropParams) (*backends.UndropResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if params.Name == "" {
		return nil, fmt.Errorf("undrop: database name is required")
	}

	all, err := b.scanPreserved()
	if err != nil {
		return nil, err
	}

	var candidates []backends.DroppedDatabase
	for _, d := range all {
		if d.Name == params.Name {
			candidates = append(candidates, d)
		}
	}

	if len(candidates) == 0 {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("undrop: no dropped database named %q; %s", params.Name, availableHint(all)))
	}

	var chosen backends.DroppedDatabase
	if params.DropID != "" {
		found := false
		for _, c := range candidates {
			if c.DropID == params.DropID {
				chosen, found = c, true
				break
			}
		}
		if !found {
			return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
				fmt.Errorf("undrop: database %q has no dropped copy with dropId %q", params.Name, params.DropID))
		}
	} else {
		// No dropId: restore the most recent drop. candidates preserve
		// scanPreserved's most-recently-dropped-first ordering.
		chosen = candidates[0]
	}

	target := params.Name
	if params.ToDatabase != "" {
		target = params.ToDatabase
	}

	liveDir := filepath.Join(b.dataDir, target)
	if _, ok := b.dbs[target]; ok {
		return nil, fmt.Errorf("undrop: a live database named %q already exists", target)
	}
	if _, statErr := os.Stat(liveDir); statErr == nil {
		return nil, fmt.Errorf("undrop: a live database named %q already exists", target)
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("undrop: checking target %q: %w", target, statErr)
	}

	// Restore is a copy, not a move: the preserved drop stays available so it can
	// be restored again (e.g. repeatedly with a different ToDatabase to make
	// several copies) and remains listed until the 30-day GC purges it. Copy into
	// a dot-prefixed temp dir first (excluded from ListDatabases), then rename it
	// into place so a partially-copied database is never visible as live.
	src := filepath.Join(b.preservedRoot(), chosen.Name, chosen.DropID)
	tmp, err := os.MkdirTemp(b.dataDir, ".restore-*")
	if err != nil {
		return nil, fmt.Errorf("undrop: restoring %q: %w", target, err)
	}
	if err := copyTree(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("undrop: restoring %q: %w", target, err)
	}
	if err := os.Rename(tmp, liveDir); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("undrop: restoring %q: %w", target, err)
	}

	return &backends.UndropResult{Name: target, DropID: chosen.DropID}, nil
}

// copyTree recursively copies the directory tree rooted at src into dst,
// creating dst if needed. Only directories and regular files are supported
// (Dolt NBS stores contain nothing else).
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cannot copy non-regular file %q", s)
		}
		if err := copyFile(s, d, info.Mode()); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func availableHint(all []backends.DroppedDatabase) string {
	if len(all) == 0 {
		return "there are no databases available to be undropped"
	}
	names := make([]string, 0, len(all))
	seen := map[string]bool{}
	for _, d := range all {
		if !seen[d.Name] {
			seen[d.Name] = true
			names = append(names, d.Name)
		}
	}
	return "available to undrop: " + strings.Join(names, ", ")
}
