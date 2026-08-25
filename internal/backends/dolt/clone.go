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
	"path/filepath"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/datas/pull"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
)

// DumboDBClone creates a new server-side database by cloning a remote. It pulls
// every remote branch's commits into a fresh database, sets the local branch
// heads to match the remote, and initializes each branch's working set so the
// database is immediately readable and writable. file:// and the http(s) gRPC
// transports (e.g. DoltHub) are supported; gRPC remotes use any credential
// configured via `dolt login` in the server environment.
func (b *Backend) DumboDBClone(ctx context.Context, params *backends.CloneParams) (*backends.CloneResult, error) {
	if params.As == "" {
		return nil, fmt.Errorf("dumboClone: target database name is required")
	}
	if isReservedDatabase(params.As) {
		return nil, fmt.Errorf("dumboClone: %q is a reserved database name", params.As)
	}
	if _, err := os.Stat(filepath.Join(b.dataDir, params.As)); err == nil {
		return nil, fmt.Errorf("dumboClone: database %q already exists", params.As)
	}

	ru, err := parseRemoteURL(params.From)
	if err != nil {
		return nil, fmt.Errorf("dumboClone: %w", err)
	}
	if ru.Scheme != dbfactory.FileScheme && !isGRPCScheme(ru.Scheme) {
		return nil, fmt.Errorf("dumboClone: unsupported remote scheme %q (supported: file, http, https)", ru.Scheme)
	}

	remoteParams, err := remoteDBParams(ru.Scheme)
	if err != nil {
		return nil, fmt.Errorf("dumboClone: %w", err)
	}

	nbf := dolttypes.Format_DOLT
	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, nbf, ru.Raw, filesys.LocalFS, remoteParams)
	if err != nil {
		return nil, fmt.Errorf("dumboClone: opening remote: %w", err)
	}
	defer func() { _ = remoteDB.Close() }()

	branchRefs, err := remoteDB.GetBranches(ctx)
	if err != nil {
		return nil, fmt.Errorf("dumboClone: listing remote branches: %w", err)
	}
	if len(branchRefs) == 0 {
		return nil, fmt.Errorf("dumboClone: remote %q has no branches", ru.Raw)
	}

	state, err := b.getOrOpenDB(ctx, params.As, true)
	if err != nil {
		return nil, fmt.Errorf("dumboClone: creating database %q: %w", params.As, err)
	}

	tempDir, err := os.MkdirTemp(b.dataDir, ".clone-*")
	if err != nil {
		return nil, fmt.Errorf("dumboClone: creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	branchNames := make([]string, 0, len(branchRefs))
	for _, br := range branchRefs {
		branchNames = append(branchNames, br.GetPath())
	}
	defaultName := pickDefaultBranch(branchNames)

	branches := make([]string, 0, len(branchRefs))
	defaultCommit := ""

	for _, br := range branchRefs {
		name := br.GetPath()

		remoteCommit, err := remoteDB.ResolveCommitRef(ctx, br)
		if err != nil {
			return nil, fmt.Errorf("dumboClone: resolving remote branch %q: %w", name, err)
		}
		ch, err := remoteCommit.HashOf()
		if err != nil {
			return nil, err
		}

		err = actions.FetchCommit(ctx, tempDir, remoteDB, state.doltDB, remoteCommit, nil)
		if err != nil && !errors.Is(err, pull.ErrDBUpToDate) && !errors.Is(err, doltdb.ErrUpToDate) {
			return nil, fmt.Errorf("dumboClone: fetching branch %q: %w", name, err)
		}

		ds, err := state.datasDB.GetDataset(ctx, "refs/heads/"+name)
		if err != nil {
			return nil, fmt.Errorf("dumboClone: getting dataset for %q: %w", name, err)
		}
		if _, err := state.datasDB.SetHead(ctx, ds, ch, ""); err != nil {
			return nil, fmt.Errorf("dumboClone: setting head for %q: %w", name, err)
		}

		// Initialize the branch working set from the cloned commit so the new
		// database is immediately readable and writable on this branch.
		localCommit, err := state.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef(name))
		if err != nil {
			return nil, fmt.Errorf("dumboClone: resolving cloned branch %q: %w", name, err)
		}
		rv, err := localCommit.GetRootValue(ctx)
		if err != nil {
			return nil, fmt.Errorf("dumboClone: reading root for %q: %w", name, err)
		}
		wsRef := ref.NewWorkingSetRef("heads/" + name)
		ws := doltdb.EmptyWorkingSet(wsRef).WithWorkingRoot(rv).WithStagedRoot(rv)
		if err := updateWorkingSet(ctx, state.doltDB, ws, name); err != nil {
			return nil, fmt.Errorf("dumboClone: initializing working set for %q: %w", name, err)
		}

		branches = append(branches, name)
		if name == defaultName {
			defaultCommit = ch.String()
		}
	}

	return &backends.CloneResult{
		DB:            params.As,
		URL:           ru.Raw,
		DefaultBranch: defaultName,
		Commit:        defaultCommit,
		Branches:      branches,
	}, nil
}

// pickDefaultBranch chooses the default branch of a freshly cloned database from
// the remote's branch set: prefer main, then master, otherwise the first branch
// listed. names is assumed non-empty (callers reject a branchless remote).
func pickDefaultBranch(names []string) string {
	for _, preferred := range []string{defaultBranch, "master"} {
		for _, n := range names {
			if n == preferred {
				return preferred
			}
		}
	}
	return names[0]
}
