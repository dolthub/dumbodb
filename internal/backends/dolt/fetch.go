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

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/datas/pull"

	"github.com/dolthub/dumbodb/internal/backends"
)

// DumboDBFetch fetches every branch from a configured remote into local
// remote-tracking refs refs/remotes/<remote>/<branch> (git fetch semantics). It
// pulls novel chunks but does not touch any local branch head. Reuses Dolt's
// actions.FetchCommit (no DoltEnv/RepoState).
func (b *Backend) DumboDBFetch(ctx context.Context, params *backends.FetchParams) (*backends.FetchResult, error) {
	remote := params.Remote
	if remote == "" {
		pull, err := b.getBranchPull(ctx, params.DBName, defaultBranch)
		if err != nil {
			return nil, err
		}
		if !pull.hasUpstream() {
			return nil, fmt.Errorf("dumboFetch: no upstream configured for branch %q; specify a remote with 'from'", defaultBranch)
		}
		remote = pull.remote
	}

	ru, err := b.resolveRemoteURL(ctx, params.DBName, remote)
	if err != nil {
		return nil, err
	}
	if !ru.supported() {
		return nil, fmt.Errorf("dumboFetch: remote scheme %q is not yet supported for fetch", ru.Scheme)
	}

	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, err
	}
	nbf := state.doltDB.Format()

	remoteParams, err := b.remoteDBParams(ru.Scheme)
	if err != nil {
		return nil, fmt.Errorf("dumboFetch: %w", err)
	}

	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, nbf, ru.Raw, filesys.LocalFS, remoteParams)
	if err != nil {
		return nil, fmt.Errorf("dumboFetch: opening remote %q: %w", remote, err)
	}
	defer func() { _ = remoteDB.Close() }()

	branchRefs, err := remoteDB.GetBranches(ctx)
	if err != nil {
		return nil, fmt.Errorf("dumboFetch: listing remote branches: %w", err)
	}
	if len(branchRefs) == 0 {
		return nil, fmt.Errorf("dumboFetch: remote %q has no branches", remote)
	}

	tempDir, err := os.MkdirTemp(b.dataDir, ".fetch-*")
	if err != nil {
		return nil, fmt.Errorf("dumboFetch: creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	fetched := make([]backends.FetchedRef, 0, len(branchRefs))
	for _, br := range branchRefs {
		name := br.GetPath()
		trackingRef := ref.NewRemoteRef(remote, name)

		// The tracking-ref head before the fetch, for the before->after report.
		// Empty when this fetch creates the tracking ref.
		var commitBefore string
		if tc, err := state.doltDB.ResolveCommitRef(ctx, trackingRef); err == nil {
			if h, err := tc.HashOf(); err == nil {
				commitBefore = h.String()
			}
		}

		remoteCommit, err := remoteDB.ResolveCommitRef(ctx, br)
		if err != nil {
			return nil, fmt.Errorf("dumboFetch: resolving remote branch %q: %w", name, err)
		}
		ch, err := remoteCommit.HashOf()
		if err != nil {
			return nil, err
		}

		err = actions.FetchCommit(ctx, tempDir, remoteDB, state.doltDB, remoteCommit, nil)
		if err != nil && !errors.Is(err, pull.ErrDBUpToDate) && !errors.Is(err, doltdb.ErrUpToDate) {
			return nil, fmt.Errorf("dumboFetch: fetching branch %q: %w", name, err)
		}

		// Update the local remote-tracking ref for this branch.
		if err := state.doltDB.SetHead(ctx, trackingRef, ch); err != nil {
			return nil, fmt.Errorf("dumboFetch: updating tracking ref for %q: %w", name, err)
		}

		if commitBefore != ch.String() {
			fetched = append(fetched, backends.FetchedRef{Branch: name, CommitBefore: commitBefore, Commit: ch.String()})
		}
	}

	return &backends.FetchResult{
		Remote:   remote,
		URL:      ru.Raw,
		Branches: fetched,
	}, nil
}
