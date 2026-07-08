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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
)

// quarantineDirName is the data-dir subdirectory holding soft-deleted databases.
// The leading dot keeps it out of the database namespace: MongoDB database names
// cannot contain '.', so no live database can ever collide with it, and
// ListDatabases skips dot-prefixed entries.
//
// Layout: <dataDir>/.dumbodb_dropped_databases/<dbName>/<dropId>/
// where dropId is the UnixNano instant of the drop. The two-level layout lets a
// single database be dropped repeatedly without losing earlier drops.
const quarantineDirName = ".dumbodb_dropped_databases"

func (b *Backend) quarantineRoot() string {
	return filepath.Join(b.dataDir, quarantineDirName)
}

// quarantineDest reserves and returns a fresh quarantine directory for a drop of
// name. The parent (<quarantineRoot>/<name>) is created; the returned leaf does
// not yet exist and is the rename target for the live database directory.
//
// Caller must hold b.mu.
func (b *Backend) quarantineDest(name string) (string, error) {
	parent := filepath.Join(b.quarantineRoot(), name)
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

// ListDroppedDatabases returns every quarantined drop, most recent first.
func (b *Backend) ListDroppedDatabases(_ context.Context) (*backends.DroppedDatabasesResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	dropped, err := b.scanQuarantine()
	if err != nil {
		return nil, err
	}
	return &backends.DroppedDatabasesResult{Databases: dropped}, nil
}

// scanQuarantine walks the quarantine directory and returns its drops sorted by
// drop id descending (most recently dropped first). Caller must hold b.mu.
func (b *Backend) scanQuarantine() ([]backends.DroppedDatabase, error) {
	root := b.quarantineRoot()
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

	all, err := b.scanQuarantine()
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
		// scanQuarantine's most-recently-dropped-first ordering.
		chosen = candidates[0]
	}

	liveDir := filepath.Join(b.dataDir, params.Name)
	if _, ok := b.dbs[params.Name]; ok {
		return nil, fmt.Errorf("undrop: a live database named %q already exists", params.Name)
	}
	if _, statErr := os.Stat(liveDir); statErr == nil {
		return nil, fmt.Errorf("undrop: a live database named %q already exists", params.Name)
	}

	src := filepath.Join(b.quarantineRoot(), chosen.Name, chosen.DropID)
	if err := os.Rename(src, liveDir); err != nil {
		return nil, fmt.Errorf("undrop: restoring %q: %w", params.Name, err)
	}

	// Remove the now-empty per-name quarantine directory if this was the last drop.
	parent := filepath.Join(b.quarantineRoot(), chosen.Name)
	if remaining, rerr := os.ReadDir(parent); rerr == nil && len(remaining) == 0 {
		_ = os.Remove(parent)
	}

	return &backends.UndropResult{Name: chosen.Name, DropID: chosen.DropID}, nil
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
