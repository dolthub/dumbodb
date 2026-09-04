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
// configuration, one document per branch keyed by "<db>.<branch>".
const branchesCollection = "system.branches"

// branchPull is a branch's fetch/merge configuration.
type branchPull struct {
	remote string
	branch string
	rebase string
	ff     string
}

func (p branchPull) empty() bool {
	return p.remote == "" && p.branch == "" && p.rebase == "" && p.ff == ""
}

// hasUpstream reports whether a fetch remote is configured.
func (p branchPull) hasUpstream() bool { return p.remote != "" }

// branchPush is a branch's persistent push target.
type branchPush struct {
	remote string
	branch string
}

func (p branchPush) empty() bool    { return p.remote == "" && p.branch == "" }
func (p branchPush) complete() bool { return p.remote != "" && p.branch != "" }

// branchConfig is the full stored configuration for one branch.
type branchConfig struct {
	pull branchPull
	push branchPush
}

func (c branchConfig) empty() bool { return c.pull.empty() && c.push.empty() }

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

// readBranchConfig returns the stored configuration for a branch.
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
		if cv, _ := doc.Get("config"); cv != nil {
			if cfgDoc, ok := cv.(*types.Document); ok {
				if pl, _ := cfgDoc.Get("pull"); pl != nil {
					if plDoc, ok := pl.(*types.Document); ok {
						cfg.pull.remote = docString(plDoc, "remote")
						cfg.pull.branch = docString(plDoc, "branch")
						cfg.pull.rebase = docString(plDoc, "rebase")
						cfg.pull.ff = docString(plDoc, "ff")
					}
				}
				if ps, _ := cfgDoc.Get("push"); ps != nil {
					if psDoc, ok := ps.(*types.Document); ok {
						cfg.push.remote = docString(psDoc, "remote")
						cfg.push.branch = docString(psDoc, "branch")
					}
				}
			}
		}
		return cfg, true, nil
	}
	return branchConfig{}, false, nil
}

func docString(doc *types.Document, key string) string {
	v, _ := doc.Get(key)
	s, _ := v.(string)
	return s
}

// writeBranchConfig replaces the branch's config document.
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

	config := must.NotFail(types.NewDocument())
	if !cfg.pull.empty() {
		pl := must.NotFail(types.NewDocument())
		setIfNotEmpty(pl, "remote", cfg.pull.remote)
		setIfNotEmpty(pl, "branch", cfg.pull.branch)
		setIfNotEmpty(pl, "rebase", cfg.pull.rebase)
		setIfNotEmpty(pl, "ff", cfg.pull.ff)
		config.Set("pull", pl)
	}
	if !cfg.push.empty() {
		ps := must.NotFail(types.NewDocument())
		setIfNotEmpty(ps, "remote", cfg.push.remote)
		setIfNotEmpty(ps, "branch", cfg.push.branch)
		config.Set("push", ps)
	}

	doc := must.NotFail(types.NewDocument(
		"_id", id,
		"branch", branch,
		"db", dbName,
		"config", config,
	))
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		return err
	}
	return nil
}

func setIfNotEmpty(doc *types.Document, key, val string) {
	if val != "" {
		doc.Set(key, val)
	}
}

// getBranchPull returns a branch's config.pull (zero value when unset).
func (b *Backend) getBranchPull(ctx context.Context, dbName, branch string) (branchPull, error) {
	cfg, ok, err := b.readBranchConfig(ctx, dbName, branch)
	if err != nil || !ok {
		return branchPull{}, err
	}
	return cfg.pull, nil
}

// getBranchPush returns a branch's config.push (zero value when unset).
func (b *Backend) getBranchPush(ctx context.Context, dbName, branch string) (branchPush, error) {
	cfg, ok, err := b.readBranchConfig(ctx, dbName, branch)
	if err != nil || !ok {
		return branchPush{}, err
	}
	return cfg.push, nil
}

// applyBranchConfig merges an update into a branch's stored config and writes it
// back, returning the resulting config.
func (b *Backend) applyBranchConfig(ctx context.Context, dbName, branch string, up *backends.BranchConfigUpdate) (branchConfig, error) {
	cfg, _, err := b.readBranchConfig(ctx, dbName, branch)
	if err != nil {
		return branchConfig{}, err
	}

	if up.UnsetPull {
		cfg.pull = branchPull{}
	}
	if up.UnsetPush {
		cfg.push = branchPush{}
	}
	applyStr(&cfg.pull.remote, up.PullRemote)
	applyStr(&cfg.pull.branch, up.PullBranch)
	applyStr(&cfg.pull.rebase, up.PullRebase)
	applyStr(&cfg.pull.ff, up.PullFF)
	applyStr(&cfg.push.remote, up.PushRemote)
	applyStr(&cfg.push.branch, up.PushBranch)

	if err := b.validateBranchConfig(ctx, dbName, cfg); err != nil {
		return branchConfig{}, err
	}
	if err := b.writeBranchConfig(ctx, dbName, branch, cfg); err != nil {
		return branchConfig{}, err
	}
	return cfg, nil
}

func applyStr(dst *string, src *string) {
	if src != nil {
		*dst = *src
	}
}

// validateBranchConfig enforces the config.{pull,push} invariants on a resulting
// config before it is written.
func (b *Backend) validateBranchConfig(ctx context.Context, dbName string, cfg branchConfig) error {
	if (cfg.pull.rebase != "" || cfg.pull.ff != "") && !cfg.pull.hasUpstream() {
		return fmt.Errorf("config.pull rebase/ff requires an upstream; set config.pull.remote first")
	}
	if !cfg.push.empty() && !cfg.push.complete() {
		return fmt.Errorf("config.push must set both remote and branch, or neither")
	}
	for _, remote := range []string{cfg.pull.remote, cfg.push.remote} {
		if remote == "" {
			continue
		}
		known, err := b.remoteExists(ctx, dbName, remote)
		if err != nil {
			return err
		}
		if !known {
			return fmt.Errorf("remote %q is not configured for database %q", remote, dbName)
		}
	}
	return nil
}
