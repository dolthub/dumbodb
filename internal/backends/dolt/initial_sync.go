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
	"strings"

	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
)

func (b *Backend) ResetInitialSyncData(ctx context.Context) error {
	entries, err := os.ReadDir(b.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		state, err := b.getOrOpenDB(ctx, entry.Name(), false)
		if err != nil {
			return fmt.Errorf("opening %s for initial sync reset: %w", entry.Name(), err)
		}
		if state == nil {
			continue
		}
		state.mu.Lock()
		initialCommit, err := initialCommitHash(ctx, state)
		state.mu.Unlock()
		if err != nil {
			return fmt.Errorf("finding %s initial commit: %w", entry.Name(), err)
		}
		var preserveCollections []string
		if entry.Name() == "admin" {
			preserveCollections = []string{backends.ReservedReplicationControlName}
		}
		_, err = b.dumboDBReset(ctx, &backends.ResetParams{
			DBName: entry.Name(), Branch: defaultBranch, CommitID: initialCommit.String(), Hard: true,
		}, preserveCollections)
		if err != nil {
			return fmt.Errorf("resetting %s initial sync data: %w", entry.Name(), err)
		}
	}
	return nil
}

func initialCommitHash(ctx context.Context, state *dbState) (hash.Hash, error) {
	current, err := resolveRootishToCommitHash(ctx, state, defaultBranch)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("resolving initial sync base: %w", err)
	}
	for {
		commit, err := datas.LoadCommitAddr(ctx, state.vs, current)
		if err != nil {
			return hash.Hash{}, fmt.Errorf("loading initial sync ancestor: %w", err)
		}
		parents, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
		if err != nil {
			return hash.Hash{}, fmt.Errorf("reading initial sync ancestors: %w", err)
		}
		if len(parents) == 0 {
			return current, nil
		}
		current = parents[0]
	}
}
