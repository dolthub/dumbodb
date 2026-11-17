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
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
)

// Status and ListDatabases both list the tables of the cached working root
// that every request shares, so they must be safe to run at the same time.
// Run with -race.
func TestStatusAndListDatabasesShareRootsSafely(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	ctx, _ := ctxWithSessionAs(t, be, "shared-root", "")
	db, err := be.Database("victim")
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}))

	const goroutines, calls = 2, 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < calls; j++ {
				_, err := be.Status(ctx, &backends.StatusParams{})
				require.NoError(t, err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < calls; j++ {
				_, err := be.ListDatabases(ctx, &backends.ListDatabasesParams{})
				require.NoError(t, err)
			}
		}()
	}
	close(start)
	wg.Wait()
}
