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

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/concurrentmap"
)

type repoStateAdapter struct {
	branch string
}

func newRepoStateAdapter(branch string) *repoStateAdapter {
	return &repoStateAdapter{branch: branch}
}

var (
	_ env.RepoStateReader[context.Context] = (*repoStateAdapter)(nil)
	_ env.RepoStateWriter                  = (*repoStateAdapter)(nil)
)

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
	return "", nil
}

func (r *repoStateAdapter) UpdateBranch(_ string, _ env.BranchConfig) error {
	return nil
}

// sqlCtxRepoStateAdapter satisfies env.RepoStateReadWriter[*sql.Context]
// for dsess's SessionDatabaseBranchSpec; methods delegate to the
// context.Context variant.
type sqlCtxRepoStateAdapter struct {
	inner *repoStateAdapter
}

var (
	_ env.RepoStateReader[*sql.Context]     = (*sqlCtxRepoStateAdapter)(nil)
	_ env.RepoStateWriter                   = (*sqlCtxRepoStateAdapter)(nil)
	_ env.RepoStateReadWriter[*sql.Context] = (*sqlCtxRepoStateAdapter)(nil)
)

func newSqlCtxRepoStateAdapter(branch string) *sqlCtxRepoStateAdapter {
	return &sqlCtxRepoStateAdapter{inner: newRepoStateAdapter(branch)}
}

func (r *sqlCtxRepoStateAdapter) CWBHeadRef(ctx *sql.Context) (ref.DoltRef, error) {
	return r.inner.CWBHeadRef(ctx)
}

func (r *sqlCtxRepoStateAdapter) CWBHeadSpec(ctx *sql.Context) (*doltdb.CommitSpec, error) {
	return r.inner.CWBHeadSpec(ctx)
}

func (r *sqlCtxRepoStateAdapter) GetRemotes() (*concurrentmap.Map[string, env.Remote], error) {
	return r.inner.GetRemotes()
}

func (r *sqlCtxRepoStateAdapter) GetBackups() (*concurrentmap.Map[string, env.Remote], error) {
	return r.inner.GetBackups()
}

func (r *sqlCtxRepoStateAdapter) GetBranches() (*concurrentmap.Map[string, env.BranchConfig], error) {
	return r.inner.GetBranches()
}

func (r *sqlCtxRepoStateAdapter) SetCWBHeadRef(ctx context.Context, m ref.MarshalableRef) error {
	return r.inner.SetCWBHeadRef(ctx, m)
}

func (r *sqlCtxRepoStateAdapter) AddRemote(rem env.Remote) error {
	return r.inner.AddRemote(rem)
}

func (r *sqlCtxRepoStateAdapter) AddBackup(rem env.Remote) error {
	return r.inner.AddBackup(rem)
}

func (r *sqlCtxRepoStateAdapter) RemoveRemote(ctx context.Context, name string) error {
	return r.inner.RemoveRemote(ctx, name)
}

func (r *sqlCtxRepoStateAdapter) RemoveBackup(ctx context.Context, name string) error {
	return r.inner.RemoveBackup(ctx, name)
}

func (r *sqlCtxRepoStateAdapter) TempTableFilesDir() (string, error) {
	return r.inner.TempTableFilesDir()
}

func (r *sqlCtxRepoStateAdapter) UpdateBranch(name string, cfg env.BranchConfig) error {
	return r.inner.UpdateBranch(name, cfg)
}
