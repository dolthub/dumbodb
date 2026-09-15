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
	_, err = dumboDBBranchCreate(ctx, state, &backends.BranchParams{
		DBName: database, Action: "add", From: defaultBranch, Name: branch,
	})
	return err
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
