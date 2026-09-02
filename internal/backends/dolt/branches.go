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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// branchesCollection is the admin-database collection that stores per-branch
// configuration, one document per branch keyed by "<db>.<branch>": the upstream
// tracking {remote, ref} and, for a tracking branch, a pull policy {rebase, ff}.
// It mirrors the identity model of system.remotes and system.users.
const branchesCollection = "system.branches"

// upstream is the tracked {remote, ref} a branch pushes to / fetches from.
type upstream struct {
	remote string
	ref    string
}

// pullPolicy is a tracking branch's persistent pull behavior, the analog of
// git's branch.<name>.rebase and pull.ff. Empty strings mean "unset" (use the
// default). rebase is "", "true", or "merges"; ff is "", "no", or "only".
type pullPolicy struct {
	rebase string
	ff     string
}

func (p pullPolicy) empty() bool { return p.rebase == "" && p.ff == "" }

// branchConfig is the full stored configuration for one branch.
type branchConfig struct {
	upstream *upstream
	pull     pullPolicy
}

func (c branchConfig) empty() bool { return c.upstream == nil && c.pull.empty() }

// branchID builds the db-qualified document key for a branch.
func branchID(dbName, branch string) string {
	return dbName + "." + branch
}

func (b *Backend) branchesColl() (backends.Collection, error) {
	adminDB, err := b.Database("admin")
	if err != nil {
		return nil, err
	}
	return adminDB.Collection(branchesCollection)
}

// readBranchConfig returns the stored configuration for a branch; ok is false
// when the branch has no config document.
func (b *Backend) readBranchConfig(ctx context.Context, dbName, branch string) (branchConfig, bool, error) {
	coll, err := b.branchesColl()
	if err != nil {
		return branchConfig{}, false, err
	}

	id := branchID(dbName, branch)
	qr, err := coll.Query(ctx, &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("_id", id)),
	})
	if err != nil {
		return branchConfig{}, false, err
	}
	defer qr.Iter.Close()

	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return branchConfig{}, false, err
		}
		if idv, _ := doc.Get("_id"); idv != id {
			continue
		}

		var cfg branchConfig
		if up, _ := doc.Get("upstream"); up != nil {
			if upDoc, ok := up.(*types.Document); ok {
				remote, _ := upDoc.Get("remote")
				refv, _ := upDoc.Get("ref")
				rs, _ := remote.(string)
				rf, _ := refv.(string)
				if rs != "" {
					cfg.upstream = &upstream{remote: rs, ref: rf}
				}
			}
		}
		if pl, _ := doc.Get("pull"); pl != nil {
			if plDoc, ok := pl.(*types.Document); ok {
				rb, _ := plDoc.Get("rebase")
				ff, _ := plDoc.Get("ff")
				cfg.pull.rebase, _ = rb.(string)
				cfg.pull.ff, _ = ff.(string)
			}
		}
		return cfg, true, nil
	}
	return branchConfig{}, false, nil
}

// writeBranchConfig replaces the branch's config document. An empty config
// deletes the document. Upsert is a delete-then-insert on the "<db>.<branch>"
// key.
func (b *Backend) writeBranchConfig(ctx context.Context, dbName, branch string, cfg branchConfig) error {
	coll, err := b.branchesColl()
	if err != nil {
		return err
	}

	id := branchID(dbName, branch)
	if _, err := coll.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{id}}); err != nil {
		return err
	}
	if cfg.empty() {
		return nil
	}

	doc := must.NotFail(types.NewDocument(
		"_id", id,
		"branch", branch,
		"db", dbName,
	))
	if cfg.upstream != nil {
		doc.Set("upstream", must.NotFail(types.NewDocument(
			"remote", cfg.upstream.remote,
			"ref", cfg.upstream.ref,
		)))
	}
	if !cfg.pull.empty() {
		pl := must.NotFail(types.NewDocument())
		if cfg.pull.rebase != "" {
			pl.Set("rebase", cfg.pull.rebase)
		}
		if cfg.pull.ff != "" {
			pl.Set("ff", cfg.pull.ff)
		}
		doc.Set("pull", pl)
	}
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		return err
	}
	return nil
}

// getUpstream returns the recorded upstream for a branch, ok=false when none is
// configured.
func (b *Backend) getUpstream(ctx context.Context, dbName, branch string) (upstream, bool, error) {
	cfg, ok, err := b.readBranchConfig(ctx, dbName, branch)
	if err != nil || !ok || cfg.upstream == nil {
		return upstream{}, false, err
	}
	return *cfg.upstream, true, nil
}

// setUpstream records (or replaces) the upstream for a branch, preserving any
// existing pull policy.
func (b *Backend) setUpstream(ctx context.Context, dbName, branch string, up upstream) error {
	cfg, _, err := b.readBranchConfig(ctx, dbName, branch)
	if err != nil {
		return err
	}
	cfg.upstream = &up
	return b.writeBranchConfig(ctx, dbName, branch, cfg)
}

// getPullPolicy returns a branch's stored pull policy (zero value when unset).
func (b *Backend) getPullPolicy(ctx context.Context, dbName, branch string) (pullPolicy, error) {
	cfg, ok, err := b.readBranchConfig(ctx, dbName, branch)
	if err != nil || !ok {
		return pullPolicy{}, err
	}
	return cfg.pull, nil
}

// setPullPolicy applies the given rebase/ff values to a branch's pull policy.
// Only non-nil values are changed; an empty-string value clears that key. The
// branch must already track an upstream.
func (b *Backend) setPullPolicy(ctx context.Context, dbName, branch string, rebase, ff *string) error {
	cfg, _, err := b.readBranchConfig(ctx, dbName, branch)
	if err != nil {
		return err
	}
	if cfg.upstream == nil {
		return fmt.Errorf("branch %q has no upstream; a pull policy applies only to a tracking branch", branch)
	}
	if rebase != nil {
		cfg.pull.rebase = *rebase
	}
	if ff != nil {
		cfg.pull.ff = *ff
	}
	return b.writeBranchConfig(ctx, dbName, branch, cfg)
}

// unsetPullPolicy clears the named pull-policy keys ("rebase", "ff") on a branch.
func (b *Backend) unsetPullPolicy(ctx context.Context, dbName, branch string, keys []string) error {
	cfg, ok, err := b.readBranchConfig(ctx, dbName, branch)
	if err != nil || !ok {
		return err
	}
	for _, k := range keys {
		switch k {
		case "rebase":
			cfg.pull.rebase = ""
		case "ff":
			cfg.pull.ff = ""
		}
	}
	return b.writeBranchConfig(ctx, dbName, branch, cfg)
}
