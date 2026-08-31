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
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/datas/pull"

	"github.com/dolthub/dumbodb/internal/backends"
)

// DumboDBPush pushes a commit to a branch on a configured remote, reusing Dolt's
// actions.Push (no DoltEnv/RepoState). It mirrors git push: an optional refspec
// [+]<src>[:<dst>] selects the source revision and destination branch, and a
// bare call pushes the connection branch to its upstream. The remote's
// refs/heads/<dst> is advanced and the local tracking ref
// refs/remotes/<remote>/<dst> is updated.
func (b *Backend) DumboDBPush(ctx context.Context, params *backends.PushParams) (*backends.PushResult, error) {
	connBranch := params.ConnBranch
	if connBranch == "" {
		connBranch = defaultBranch
	}

	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, err
	}

	// Parse the refspec into: srcRev (a commit-ish resolved below), localBranch
	// (the branch the source names, empty when the source is a bare revision that
	// cannot be tracked), dst (destination branch on the remote), forcePlus (a
	// leading '+'), and explicit (a refspec was given, which skips the gate).
	srcRev, localBranch, dst, forcePlus, explicit, err := parsePushSpec(ctx, state.doltDB, connBranch, params.RefSpec)
	if err != nil {
		return nil, err
	}

	// Resolve the remote the git way (push.default=simple). A branch's "own
	// remote" is the remote it tracks (its upstream), defaulting to origin when
	// untracked. Only a bare push to the branch's own remote demands a matching,
	// same-named upstream; a bare push to any other remote is a triangular
	// current-branch push. An explicit refspec already has its destination and
	// skips the gate.
	remote := params.Remote
	var up upstream
	hasUpstream := false
	if localBranch != "" {
		up, hasUpstream, err = b.getUpstream(ctx, params.DBName, localBranch)
		if err != nil {
			return nil, err
		}
	}

	if explicit {
		// git push <remote> <refspec>: a missing 'to' falls back to the source
		// branch's upstream remote.
		if remote == "" {
			if !hasUpstream {
				return nil, fmt.Errorf("dumboPush: no remote given; specify 'to'")
			}
			remote = up.remote
		}
	} else {
		// Bare push (git push): the upstream drives both the remote and the dst.
		if remote == "" {
			if !hasUpstream {
				return nil, fmt.Errorf("dumboPush: no remote given and branch %q has no upstream; specify 'to' or push with a refSpec", localBranch)
			}
			remote = up.remote
		}
		ownRemote := defaultRemote
		if hasUpstream {
			ownRemote = up.remote
		}
		if remote == ownRemote {
			if !hasUpstream {
				return nil, fmt.Errorf("dumboPush: branch %q has no upstream; specify a different remote or push with a refSpec", localBranch)
			}
			// git simple refuses a bare push when the upstream branch's name
			// differs from the local branch; push explicitly instead.
			if up.ref != localBranch {
				return nil, fmt.Errorf("dumboPush: branch %q tracks %s/%s and their names differ; push explicitly, e.g. refSpec %q", localBranch, up.remote, up.ref, localBranch+":"+up.ref)
			}
			dst = up.ref
		} else {
			dst = localBranch // triangular: same-named branch on the other remote
		}
	}

	ru, err := b.resolveRemoteURL(ctx, params.DBName, remote)
	if err != nil {
		return nil, err
	}
	if !ru.supported() {
		return nil, fmt.Errorf("dumboPush: remote scheme %q is not yet supported for push", ru.Scheme)
	}

	// Resolve the source revision (branch, HEAD, HEAD~3, main~2, a hash) to the
	// commit to push. HEAD-relative specs resolve against the connection branch.
	cs, err := doltdb.NewCommitSpec(srcRev)
	if err != nil {
		return nil, fmt.Errorf("dumboPush: invalid source %q: %w", srcRev, err)
	}
	optCmt, err := state.doltDB.Resolve(ctx, cs, ref.NewBranchRef(connBranch))
	if err != nil {
		return nil, fmt.Errorf("dumboPush: resolving %q: %w", srcRev, err)
	}
	commit, ok := optCmt.ToCommit()
	if !ok {
		return nil, fmt.Errorf("dumboPush: source %q did not resolve to a commit", srcRev)
	}
	commitHash, err := commit.HashOf()
	if err != nil {
		return nil, err
	}

	nbf := state.doltDB.Format()

	// PrepareDB creates local stores on demand and inits a git+file bare repo,
	// but is unsupported for gRPC (http/https) and git+http/https/ssh remotes,
	// which must already exist. Skip it for those.
	if !isGRPCScheme(ru.Scheme) && !(isGitScheme(ru.Scheme) && !gitPreparable(ru.Scheme)) {
		if err := dbfactory.PrepareDB(ctx, nbf, ru.Raw, nil); err != nil {
			return nil, fmt.Errorf("dumboPush: preparing remote %q: %w", remote, err)
		}
	}

	remoteParams, err := b.remoteDBParams(ru.Scheme)
	if err != nil {
		return nil, fmt.Errorf("dumboPush: %w", err)
	}

	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, nbf, ru.Raw, filesys.LocalFS, remoteParams)
	if err != nil {
		return nil, fmt.Errorf("dumboPush: opening remote %q: %w", remote, err)
	}
	defer func() { _ = remoteDB.Close() }()

	tempDir, err := os.MkdirTemp(b.dataDir, ".push-*")
	if err != nil {
		return nil, fmt.Errorf("dumboPush: creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	mode := ref.FastForwardOnly
	if params.Force || forcePlus {
		mode = ref.ForceUpdate
	}

	// The push sends the source commit to refs/heads/<dst> on the remote and
	// tracks it at refs/remotes/<remote>/<dst>.
	destBranchRef := ref.NewBranchRef(dst)
	remoteRef := ref.NewRemoteRef(remote, dst)

	// The remote branch head before the push, for the before->after report.
	// Empty when the push creates the branch on the remote.
	var commitBefore string
	if bc, err := remoteDB.ResolveCommitRef(ctx, destBranchRef); err == nil {
		if h, err := bc.HashOf(); err == nil {
			commitBefore = h.String()
		}
	}

	// statsCh may be nil; the puller guards against a nil channel.
	err = actions.Push(ctx, tempDir, mode, destBranchRef, remoteRef, state.doltDB, remoteDB, commit, nil)
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
		if err := remoteDB.SetHead(ctx, destBranchRef, commitHash); err != nil {
			return nil, fmt.Errorf("dumboPush: setting remote ref: %w", err)
		}
		if err := state.doltDB.SetHead(ctx, remoteRef, commitHash); err != nil {
			return nil, fmt.Errorf("dumboPush: updating local tracking ref: %w", err)
		}
	}

	// Only -u records the upstream (git push -u); a plain push never changes it.
	// The tracked ref is the destination branch, which may differ from the local
	// branch when a refspec renames it. A bare revision source names no branch to
	// track, so setUpstream is an error there (git silently skips it).
	if params.SetUpstream {
		if localBranch == "" {
			return nil, fmt.Errorf("dumboPush: cannot set upstream: source %q is not a branch", srcRev)
		}
		if err := b.setUpstream(ctx, params.DBName, localBranch, upstream{remote: remote, ref: dst}); err != nil {
			return nil, fmt.Errorf("dumboPush: recording upstream: %w", err)
		}
	}

	return &backends.PushResult{
		Remote:       remote,
		URL:          ru.Raw,
		Branch:       localBranch,
		RemoteBranch: dst,
		CommitBefore: commitBefore,
		CommitPushed: commitHash.String(),
		UpToDate:     upToDate,
	}, nil
}

// parsePushSpec interprets a git-style push refspec [+]<src>[:<dst>]. For a bare
// push (empty spec) it targets the connection branch, leaving dst for the caller
// to fill from the upstream. It returns the source revision (a commit-ish for
// doltdb.NewCommitSpec), the local branch the source names (empty when the
// source is a bare revision that cannot be tracked), the destination branch, a
// force flag from a leading '+', and whether a refspec was given (explicit).
func parsePushSpec(ctx context.Context, ddb *doltdb.DoltDB, connBranch, spec string) (srcRev, localBranch, dst string, force, explicit bool, err error) {
	if spec == "" {
		return connBranch, connBranch, "", false, false, nil
	}

	explicit = true
	if strings.HasPrefix(spec, "+") {
		force = true
		spec = spec[1:]
	}

	lhs := spec
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		lhs = spec[:i]
		dst = spec[i+1:]
	}
	lhs = strings.TrimSpace(lhs)
	dst = strings.TrimSpace(dst)
	if lhs == "" {
		return "", "", "", false, false, fmt.Errorf("dumboPush: refspec %q has an empty source", spec)
	}
	srcRev = lhs

	// Identify the local branch the source names, for tracking and same-name
	// destination inference. HEAD is the connection branch; a plain branch name
	// is itself; anything else (HEAD~3, main~2, a hash) names no branch.
	if strings.EqualFold(lhs, "HEAD") {
		localBranch = connBranch
	} else if _, ok, herr := ddb.HasBranch(ctx, lhs); herr != nil {
		return "", "", "", false, false, herr
	} else if ok {
		localBranch = lhs
	}

	if dst == "" {
		// Colon-less refspec: the destination is the source branch's own name; a
		// bare revision has no branch name to use.
		if localBranch == "" {
			return "", "", "", false, false, fmt.Errorf("dumboPush: refspec %q names a commit, not a branch; use <source>:<branch>", spec)
		}
		dst = localBranch
	}
	return srcRev, localBranch, dst, force, explicit, nil
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
