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
	"io"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
)

// collChangeKind classifies a single document change between two collection maps.
type collChangeKind int

const (
	collAdded    collChangeKind = iota // present only in the new (to) map
	collRemoved                        // present only in the old (from) map
	collModified                       // present in both with a different value
)

// collChange is one changed document discovered by forEachCollectionChange.
// The key and value tuples are the raw prolly entries; From is the old value
// (nil for an added document) and To is the new value (nil for a removed one).
type collChange struct {
	kind collChangeKind
	key  val.Tuple
	from val.Tuple
	to   val.Tuple
}

// errStopCollDiff is the sentinel used to stop a diff walk early. It never
// escapes forEachCollectionChange.
var errStopCollDiff = errors.New("stop collection diff")

// forEachCollectionChange diffs two collection maps and invokes fn once per
// changed document. fromMap is the old/parent side, toMap the new/commit side.
//
// It uses prolly.DiffMaps (tree.DiffOrderedTrees), which compares internal node
// chunk addresses and skips entire shared subtrees -- so the walk is
// proportional to the number of changed documents, not the collection size.
// This is the structural-sharing property of the prolly storage model.
//
// fn returns stop=true to end iteration early (used by short-circuit callers).
// Unchanged documents are never visited; fn is responsible for decoding a
// document body only when it needs one.
func forEachCollectionChange(ctx context.Context, fromMap, toMap prolly.Map, fn func(collChange) (stop bool, err error)) error {
	err := prolly.DiffMaps(ctx, fromMap, toMap, false, func(_ context.Context, d tree.Diff) error {
		c := collChange{key: val.Tuple(d.Key)}
		switch d.Type {
		case tree.AddedDiff:
			c.kind = collAdded
			c.to = val.Tuple(d.To)
		case tree.RemovedDiff:
			c.kind = collRemoved
			c.from = val.Tuple(d.From)
		case tree.ModifiedDiff:
			c.kind = collModified
			c.from = val.Tuple(d.From)
			c.to = val.Tuple(d.To)
		default:
			return nil
		}

		stop, ferr := fn(c)
		if ferr != nil {
			return ferr
		}
		if stop {
			return errStopCollDiff
		}
		return nil
	})

	// DiffMaps reports normal completion as io.EOF; the sentinel signals an
	// intentional early stop. Neither is a real error.
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, errStopCollDiff) {
		return nil
	}
	return err
}
