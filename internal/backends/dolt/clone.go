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

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/datas/pull"
	"github.com/dolthub/dolt/go/store/hash"
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
	if !isCloneableScheme(ru.Scheme) {
		// Keep this list in sync with isCloneableScheme.
		return nil, fmt.Errorf("dumboClone: unsupported remote scheme %q; supported schemes: file, http(s), s3, gs, az, oss, oci, git+file/http/https/ssh", ru.Scheme)
	}

	remoteParams, err := b.remoteDBParams(ru.Scheme)
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

	// The remote branch that populates local main: the remote's own main by
	// default, or params.TrackAsMain when mapping a non-main default onto main.
	// Every database must have a main, so the source branch must exist.
	mainSource := defaultBranch
	if params.TrackAsMain != "" {
		mainSource = params.TrackAsMain
	}
	hasMainSource := false
	for _, br := range branchRefs {
		if br.GetPath() == mainSource {
			hasMainSource = true
			break
		}
	}
	if !hasMainSource {
		if params.TrackAsMain != "" {
			return nil, fmt.Errorf("dumboClone: remote %q has no %q branch (trackAsMain)", ru.Raw, params.TrackAsMain)
		}
		return nil, fmt.Errorf("dumboClone: remote %q has no %q branch; every database must have one -- clone with trackAsMain to map another branch onto main, or create a database and dumboFetch the branch you need", ru.Raw, defaultBranch)
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

	// initLocalBranch points refs/heads/<local> at ch and initializes its working set.
	initLocalBranch := func(local string, ch hash.Hash) error {
		ds, err := state.datasDB.GetDataset(ctx, "refs/heads/"+local)
		if err != nil {
			return fmt.Errorf("dumboClone: getting dataset for %q: %w", local, err)
		}
		if _, err := state.datasDB.SetHead(ctx, ds, ch, ""); err != nil {
			return fmt.Errorf("dumboClone: setting head for %q: %w", local, err)
		}
		localCommit, err := state.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef(local))
		if err != nil {
			return fmt.Errorf("dumboClone: resolving cloned branch %q: %w", local, err)
		}
		rv, err := localCommit.GetRootValue(ctx)
		if err != nil {
			return fmt.Errorf("dumboClone: reading root for %q: %w", local, err)
		}
		wsRef := ref.NewWorkingSetRef("heads/" + local)
		ws := doltdb.EmptyWorkingSet(wsRef).WithWorkingRoot(rv).WithStagedRoot(rv)
		if err := updateWorkingSet(ctx, state.doltDB, ws, local); err != nil {
			return fmt.Errorf("dumboClone: initializing working set for %q: %w", local, err)
		}
		return nil
	}

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

		// git clone records refs/remotes/origin/<branch> for every remote branch.
		if err := state.doltDB.SetHead(ctx, ref.NewRemoteRef("origin", name), ch); err != nil {
			return nil, fmt.Errorf("dumboClone: setting tracking ref for %q: %w", name, err)
		}

		// Map to a local branch: the mainSource branch becomes local main; a
		// remote "main" overridden by trackAsMain survives only as a tracking ref.
		local := name
		if name == mainSource {
			local = defaultBranch
		} else if name == defaultBranch {
			continue
		}
		if err := initLocalBranch(local, ch); err != nil {
			return nil, err
		}
	}

	// git clone parity: register an origin remote for the source and make main
	// track the source branch, so push/fetch with no target work on the clone.
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: params.As, Action: "add", Name: "origin", URL: ru.Raw}); err != nil {
		return nil, fmt.Errorf("dumboClone: registering origin remote: %w", err)
	}
	originName, upstreamBranch := "origin", mainSource
	if _, err := b.applyBranchConfig(ctx, params.As, defaultBranch, &backends.BranchConfigUpdate{
		PullRemote: &originName,
		PullBranch: &upstreamBranch,
	}); err != nil {
		return nil, fmt.Errorf("dumboClone: setting upstream for %q: %w", defaultBranch, err)
	}

	return &backends.CloneResult{
		DB:  params.As,
		URL: ru.Raw,
	}, nil
}
