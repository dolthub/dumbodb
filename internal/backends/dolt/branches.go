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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// branchesCollection is the admin-database collection that stores per-branch
// configuration, one document per branch keyed by "<db>.<branch>". v0 carries
// only upstream tracking. It mirrors the identity model of system.remotes and
// system.users.
const branchesCollection = "system.branches"

// upstream is the tracked {remote, ref} a branch pushes to / fetches from.
type upstream struct {
	remote string
	ref    string
}

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

// getUpstream returns the recorded upstream for a branch, ok=false when none is
// configured.
func (b *Backend) getUpstream(ctx context.Context, dbName, branch string) (upstream, bool, error) {
	coll, err := b.branchesColl()
	if err != nil {
		return upstream{}, false, err
	}

	id := branchID(dbName, branch)
	qr, err := coll.Query(ctx, &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("_id", id)),
	})
	if err != nil {
		return upstream{}, false, err
	}
	defer qr.Iter.Close()

	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return upstream{}, false, err
		}
		if idv, _ := doc.Get("_id"); idv != id {
			continue
		}
		up, _ := doc.Get("upstream")
		upDoc, ok := up.(*types.Document)
		if !ok {
			return upstream{}, false, nil
		}
		remote, _ := upDoc.Get("remote")
		refv, _ := upDoc.Get("ref")
		rs, _ := remote.(string)
		rf, _ := refv.(string)
		if rs == "" {
			return upstream{}, false, nil
		}
		return upstream{remote: rs, ref: rf}, true, nil
	}
	return upstream{}, false, nil
}

// setUpstream records (or replaces) the upstream for a branch. Upsert is a
// delete-then-insert on the "<db>.<branch>" key.
func (b *Backend) setUpstream(ctx context.Context, dbName, branch string, up upstream) error {
	coll, err := b.branchesColl()
	if err != nil {
		return err
	}

	id := branchID(dbName, branch)
	if _, err := coll.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{id}}); err != nil {
		return err
	}

	doc := must.NotFail(types.NewDocument(
		"_id", id,
		"branch", branch,
		"db", dbName,
		"upstream", must.NotFail(types.NewDocument(
			"remote", up.remote,
			"ref", up.ref,
		)),
	))
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		return err
	}
	return nil
}
