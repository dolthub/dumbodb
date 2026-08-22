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

// DumboDBPush pushes a branch's committed HEAD to a configured remote, reusing
// Dolt's actions.Push (no DoltEnv/RepoState). Client push does not remap refs:
// the remote's refs/heads/<branch> is advanced directly and the local
// remote-tracking ref refs/remotes/<remote>/<branch> is updated.
func (b *Backend) DumboDBPush(ctx context.Context, params *backends.PushParams) (*backends.PushResult, error) {
	ru, err := b.resolveRemoteURL(ctx, params.DBName, params.Remote)
	if err != nil {
		return nil, err
	}
	if !ru.supported() {
		return nil, fmt.Errorf("dumboPush: remote scheme %q is not yet supported for push", ru.Scheme)
	}

	branch := params.Branch
	if branch == "" {
		branch = defaultBranch
	}

	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, err
	}

	branchRef := ref.NewBranchRef(branch)
	commit, err := state.doltDB.ResolveCommitRef(ctx, branchRef)
	if err != nil {
		return nil, fmt.Errorf("dumboPush: resolving branch %q: %w", branch, err)
	}
	commitHash, err := commit.HashOf()
	if err != nil {
		return nil, err
	}

	nbf := state.doltDB.Format()

	if err := dbfactory.PrepareDB(ctx, nbf, ru.Raw, nil); err != nil {
		return nil, fmt.Errorf("dumboPush: preparing remote %q: %w", params.Remote, err)
	}

	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, nbf, ru.Raw, filesys.LocalFS, map[string]interface{}{
		dbfactory.DisableSingletonCacheParam: "true",
	})
	if err != nil {
		return nil, fmt.Errorf("dumboPush: opening remote %q: %w", params.Remote, err)
	}
	defer func() { _ = remoteDB.Close() }()

	tempDir, err := os.MkdirTemp(b.dataDir, ".push-*")
	if err != nil {
		return nil, fmt.Errorf("dumboPush: creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	mode := ref.FastForwardOnly
	if params.Force {
		mode = ref.ForceUpdate
	}

	remoteRef := ref.NewRemoteRef(params.Remote, branch)

	// statsCh may be nil; the puller guards against a nil channel.
	err = actions.Push(ctx, tempDir, mode, branchRef, remoteRef, state.doltDB, remoteDB, commit, nil)
	upToDate := errors.Is(err, pull.ErrDBUpToDate) || errors.Is(err, doltdb.ErrUpToDate)
	if err != nil && !upToDate {
		return nil, fmt.Errorf("dumboPush: %w", err)
	}

	if upToDate {
		// actions.Push returns before updating refs when there are no chunks to
		// transfer, so a new branch pointing at a commit already on the remote
		// would otherwise never get its ref created. The fast-forward check
		// already passed (or force was requested), so set the remote branch ref
		// and the local tracking ref to the pushed commit.
		if err := remoteDB.SetHead(ctx, branchRef, commitHash); err != nil {
			return nil, fmt.Errorf("dumboPush: setting remote ref: %w", err)
		}
		if err := state.doltDB.SetHead(ctx, remoteRef, commitHash); err != nil {
			return nil, fmt.Errorf("dumboPush: updating local tracking ref: %w", err)
		}
	}

	return &backends.PushResult{
		Remote:   params.Remote,
		URL:      ru.Raw,
		Branch:   branch,
		Commit:   commitHash.String(),
		UpToDate: upToDate,
	}, nil
}

// resolveRemoteURL looks up a remote's stored URL in admin.system.remotes and
// returns it parsed. Shared by push and (later) fetch.
func (b *Backend) resolveRemoteURL(ctx context.Context, dbName, remote string) (*remoteURL, error) {
	adminDB, err := b.Database("admin")
	if err != nil {
		return nil, err
	}
	coll, err := adminDB.Collection(remotesCollection)
	if err != nil {
		return nil, err
	}
	doc, err := findRemoteDoc(ctx, coll, remoteID(dbName, remote))
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("remote %q not found for database %q", remote, dbName)
	}
	urlVal, _ := doc.Get("url")
	urlStr, _ := urlVal.(string)
	ru, err := parseRemoteURL(urlStr)
	if err != nil {
		return nil, fmt.Errorf("stored url for remote %q is invalid: %w", remote, err)
	}
	return ru, nil
}
