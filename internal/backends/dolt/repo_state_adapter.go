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

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/concurrentmap"
)

// repoStateAdapter satisfies env.RepoStateReader[context.Context] and
// env.RepoStateWriter for a DumboDB database. dolt's sqle.NewDatabase
// requires both via env.DbData; DumboDB has no on-disk repo_state.json
// (its storage layout in dolt-storage-layout.md does not write one), so
// the adapter returns minimal defaults.
//
// Read methods return what dsess needs to drive a base / revision database
// for one DumboDB database+branch:
//   - CWBHeadRef returns refs/heads/<branch>
//   - CWBHeadSpec returns HEAD on that branch
//   - GetRemotes / GetBackups / GetBranches return empty maps (DumboDB does
//     not surface remotes / branch configs yet).
//
// Write methods are no-ops or return an error for operations DumboDB does
// not support (remotes, backups). dolt's higher layers call SetCWBHeadRef
// during checkout; we make it a no-op because DumboDB tracks the current
// branch via its rootish connection string, not via repo state.
type repoStateAdapter struct {
	branch string
}

func newRepoStateAdapter(branch string) *repoStateAdapter {
	return &repoStateAdapter{branch: branch}
}

// Compile-time assertion.
var (
	_ env.RepoStateReader[context.Context] = (*repoStateAdapter)(nil)
	_ env.RepoStateWriter                  = (*repoStateAdapter)(nil)
)

// RepoStateReader[context.Context]

func (r *repoStateAdapter) CWBHeadRef(_ context.Context) (ref.DoltRef, error) {
	return ref.NewBranchRef(r.branch), nil
}

func (r *repoStateAdapter) CWBHeadSpec(_ context.Context) (*doltdb.CommitSpec, error) {
	return doltdb.NewCommitSpec("HEAD")
}

func (r *repoStateAdapter) GetRemotes() (*concurrentmap.Map[string, env.Remote], error) {
	return concurrentmap.New[string, env.Remote](), nil
}

func (r *repoStateAdapter) GetBackups() (*concurrentmap.Map[string, env.Remote], error) {
	return concurrentmap.New[string, env.Remote](), nil
}

func (r *repoStateAdapter) GetBranches() (*concurrentmap.Map[string, env.BranchConfig], error) {
	return concurrentmap.New[string, env.BranchConfig](), nil
}

// RepoStateWriter

func (r *repoStateAdapter) SetCWBHeadRef(_ context.Context, _ ref.MarshalableRef) error {
	return nil
}

func (r *repoStateAdapter) AddRemote(_ env.Remote) error {
	return fmt.Errorf("dumbodb: remotes not supported")
}

func (r *repoStateAdapter) AddBackup(_ env.Remote) error {
	return fmt.Errorf("dumbodb: backups not supported")
}

func (r *repoStateAdapter) RemoveRemote(_ context.Context, _ string) error {
	return fmt.Errorf("dumbodb: remotes not supported")
}

func (r *repoStateAdapter) RemoveBackup(_ context.Context, _ string) error {
	return fmt.Errorf("dumbodb: backups not supported")
}

func (r *repoStateAdapter) TempTableFilesDir() (string, error) {
	// dolt uses this for staging large table imports; DumboDB does not
	// surface large imports yet, so an OS temp dir is fine.
	return "", nil
}

func (r *repoStateAdapter) UpdateBranch(_ string, _ env.BranchConfig) error {
	return nil
}
