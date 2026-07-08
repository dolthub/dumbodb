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
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
)

// droppedDatabaseGCLoop mirrors sessionSweepLoop (ticker + stop channel),
// delegating the work to purgeExpiredDroppedDatabases.
func (b *Backend) droppedDatabaseGCLoop() {
	defer close(b.droppedGCDone)

	ticker := time.NewTicker(b.droppedGCPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-b.droppedGCStop:
			return
		case <-ticker.C:
			if _, err := b.purgeExpiredDroppedDatabases(time.Now(), b.droppedGCTTL); err != nil {
				b.l.Warn("dropped-database GC sweep failed", "err", err)
			}
		}
	}
}

// purgeExpiredDroppedDatabases removes preserved drops older than maxAge (a drop's
// age comes from its dropId). now/maxAge are explicit so it is testable without the timer.
func (b *Backend) purgeExpiredDroppedDatabases(now time.Time, maxAge time.Duration) ([]backends.DroppedDatabase, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	all, err := b.scanPreserved()
	if err != nil {
		return nil, err
	}

	cutoff := now.Add(-maxAge)

	var purged []backends.DroppedDatabase
	for _, d := range all {
		droppedAt := time.Unix(0, d.DroppedAtUnixNano)
		if !droppedAt.Before(cutoff) {
			continue
		}

		if rmErr := b.removePreservedDrop(d); rmErr != nil {
			b.l.Warn("dropped-database GC: could not remove expired drop",
				"db", d.Name, "dropId", d.DropID, "err", rmErr)
			continue
		}

		b.l.Info("purged expired dropped database",
			"db", d.Name, "dropId", d.DropID,
			"droppedAt", droppedAt.UTC(), "ageDays", int(now.Sub(droppedAt).Hours()/24))
		purged = append(purged, d)
	}

	return purged, nil
}

// removePreservedDrop deletes one drop's directory (and its parent if now empty). Caller must hold b.mu.
func (b *Backend) removePreservedDrop(d backends.DroppedDatabase) error {
	dir := filepath.Join(b.preservedRoot(), d.Name, d.DropID)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}

	parent := filepath.Join(b.preservedRoot(), d.Name)
	if remaining, rerr := os.ReadDir(parent); rerr == nil && len(remaining) == 0 {
		_ = os.Remove(parent)
	}
	return nil
}

// PurgeDroppedDatabases permanently removes preserved drops matching the filter
// (the manual analog of the automatic GC).
func (b *Backend) PurgeDroppedDatabases(_ context.Context, params *backends.PurgeDroppedParams) (*backends.PurgeDroppedResult, error) {
	if params == nil || params.Name == "" {
		return nil, fmt.Errorf("purge: name is required")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	all, err := b.scanPreserved()
	if err != nil {
		return nil, err
	}

	var purged []backends.DroppedDatabase
	for _, d := range all {
		if params.Name != "" && d.Name != params.Name {
			continue
		}
		if params.DropID != "" && d.DropID != params.DropID {
			continue
		}
		if !params.DroppedBefore.IsZero() && !time.Unix(0, d.DroppedAtUnixNano).Before(params.DroppedBefore) {
			continue
		}

		if rmErr := b.removePreservedDrop(d); rmErr != nil {
			b.l.Warn("purge: could not remove dropped database",
				"db", d.Name, "dropId", d.DropID, "err", rmErr)
			continue
		}

		b.l.Info("purged dropped database",
			"db", d.Name, "dropId", d.DropID, "droppedAt", time.Unix(0, d.DroppedAtUnixNano).UTC())
		purged = append(purged, d)
	}

	return &backends.PurgeDroppedResult{Purged: purged}, nil
}
