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
	"sort"
	"strings"

	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
)

func (b *Backend) EnsureReplicationBranch(ctx context.Context, database, branch string) error {
	if database == "" || branch == "" {
		return fmt.Errorf("replication database and branch are required")
	}
	state, err := b.getOrOpenDB(ctx, database, true)
	if err != nil {
		return err
	}
	if branch == defaultBranch {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	dataset, err := state.datasDB.GetDataset(ctx, branchRefPrefix+branch)
	if err != nil {
		return err
	}
	if dataset.HasHead() {
		return nil
	}
	initialCommit, err := initialCommitHash(ctx, state)
	if err != nil {
		return err
	}
	_, err = dumboDBBranchCreate(ctx, state, &backends.BranchParams{
		DBName: database, Action: "add", From: initialCommit.String(), Name: branch,
	})
	return err
}

func (b *Backend) ResetReplicationBranch(ctx context.Context, database, branch string) error {
	if database == "" || branch == "" {
		return fmt.Errorf("replication database and branch are required")
	}
	if branch == defaultBranch {
		return fmt.Errorf("replication branch cannot be %q", defaultBranch)
	}
	state, err := b.getOrOpenDB(ctx, database, true)
	if err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	initialCommit, err := initialCommitHash(ctx, state)
	if err != nil {
		return err
	}
	dataset, err := state.datasDB.GetDataset(ctx, branchRefPrefix+branch)
	if err != nil {
		return err
	}
	if dataset.HasHead() {
		if _, err := dumboDBBranchDelete(ctx, state, &backends.BranchParams{
			DBName: database, From: defaultBranch, Name: branch, Force: true,
		}); err != nil {
			return err
		}
	}
	_, err = dumboDBBranchCreate(ctx, state, &backends.BranchParams{
		DBName: database, Action: "add", From: initialCommit.String(), Name: branch,
	})
	return err
}

func initialCommitHash(ctx context.Context, state *dbState) (hash.Hash, error) {
	current, err := resolveRootishToCommitHash(ctx, state, defaultBranch)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("resolving replication branch base: %w", err)
	}
	for {
		commit, err := datas.LoadCommitAddr(ctx, state.vs, current)
		if err != nil {
			return hash.Hash{}, fmt.Errorf("loading replication branch ancestor: %w", err)
		}
		parents, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
		if err != nil {
			return hash.Hash{}, fmt.Errorf("reading replication branch ancestors: %w", err)
		}
		if len(parents) == 0 {
			return current, nil
		}
		current = parents[0]
	}
}

func (b *Backend) ReplicationBranchExists(ctx context.Context, database, branch string) (bool, error) {
	if database == "" || branch == "" {
		return false, fmt.Errorf("replication database and branch are required")
	}
	state, err := b.getOrOpenDB(ctx, database, false)
	if err != nil || state == nil {
		return false, err
	}
	dataset, err := state.datasDB.GetDataset(ctx, branchRefPrefix+branch)
	if err != nil {
		return false, err
	}
	return dataset.HasHead(), nil
}

func (b *Backend) ListReplicationDatabases(context.Context) ([]string, error) {
	entries, err := os.ReadDir(b.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var databases []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			databases = append(databases, entry.Name())
		}
	}
	sort.Strings(databases)
	return databases, nil
}
