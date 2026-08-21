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
	"errors"
	"fmt"
	"os"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/datas/pull"

	"github.com/dolthub/dumbodb/internal/backends"
)

// DumboDBFetch fetches a branch from a configured remote into the local store
// and updates the local remote-tracking ref refs/remotes/<remote>/<branch>. Like
// git fetch, it does not touch the local branch head. Reuses Dolt's
// actions.FetchCommit (no DoltEnv/RepoState).
func (b *Backend) DumboDBFetch(ctx context.Context, params *backends.FetchParams) (*backends.FetchResult, error) {
	ru, err := b.resolveRemoteURL(ctx, params.DBName, params.Remote)
	if err != nil {
		return nil, err
	}
	if !ru.supported() {
		return nil, fmt.Errorf("dumboFetch: remote scheme %q is not yet supported for fetch", ru.Scheme)
	}

	branch := params.Branch
	if branch == "" {
		branch = defaultBranch
	}

	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, err
	}

	nbf := state.doltDB.Format()

	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, nbf, ru.Raw, filesys.LocalFS, map[string]interface{}{
		dbfactory.DisableSingletonCacheParam: "true",
	})
	if err != nil {
		return nil, fmt.Errorf("dumboFetch: opening remote %q: %w", params.Remote, err)
	}
	defer func() { _ = remoteDB.Close() }()

	remoteCommit, err := remoteDB.ResolveCommitRef(ctx, ref.NewBranchRef(branch))
	if err != nil {
		return nil, fmt.Errorf("dumboFetch: branch %q not found on remote %q: %w", branch, params.Remote, err)
	}
	remoteHash, err := remoteCommit.HashOf()
	if err != nil {
		return nil, err
	}

	tempDir, err := os.MkdirTemp(b.dataDir, ".fetch-*")
	if err != nil {
		return nil, fmt.Errorf("dumboFetch: creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	err = actions.FetchCommit(ctx, tempDir, remoteDB, state.doltDB, remoteCommit, nil)
	upToDate := errors.Is(err, pull.ErrDBUpToDate) || errors.Is(err, doltdb.ErrUpToDate)
	if err != nil && !upToDate {
		return nil, fmt.Errorf("dumboFetch: %w", err)
	}

	// Update the local remote-tracking ref to the fetched commit.
	if err := state.doltDB.SetHead(ctx, ref.NewRemoteRef(params.Remote, branch), remoteHash); err != nil {
		return nil, fmt.Errorf("dumboFetch: updating tracking ref: %w", err)
	}

	return &backends.FetchResult{
		Remote:   params.Remote,
		URL:      ru.Raw,
		Branch:   branch,
		Commit:   remoteHash.String(),
		UpToDate: upToDate,
	}, nil
}
