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
	"os"
	"path/filepath"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
)

// droppedDatabaseGCLoop periodically removes soft-deleted databases whose age
// exceeds the retention TTL. It mirrors sessionSweepLoop: a ticker plus a stop
// channel, with all real work delegated to purgeExpiredDroppedDatabases (which
// takes now/maxAge explicitly so it is testable without the timer).
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

// purgeExpiredDroppedDatabases removes every preserved drop whose drop time is
// more than maxAge before now, returning the drops that were purged. A drop's
// time is its dropId (the UnixNano recorded when it was dropped), so age is
// derived from the preserved-drops directory name -- no filesystem mtime needed.
//
// This is the testable core of the background GC: callers pass now and maxAge
// explicitly. The hourly loop passes time.Now() and the retention TTL.
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

		dir := filepath.Join(b.preservedRoot(), d.Name, d.DropID)
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			b.l.Warn("dropped-database GC: could not remove expired drop",
				"db", d.Name, "dropId", d.DropID, "err", rmErr)
			continue
		}

		b.l.Info("purged expired dropped database",
			"db", d.Name, "dropId", d.DropID,
			"droppedAt", droppedAt.UTC(), "ageDays", int(now.Sub(droppedAt).Hours()/24))
		purged = append(purged, d)

		// Remove the now-empty per-name preserved-drops directory if this was its last drop.
		parent := filepath.Join(b.preservedRoot(), d.Name)
		if remaining, rerr := os.ReadDir(parent); rerr == nil && len(remaining) == 0 {
			_ = os.Remove(parent)
		}
	}

	return purged, nil
}
