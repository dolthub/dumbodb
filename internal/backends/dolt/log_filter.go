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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"

	"github.com/dolthub/dolt/go/libraries/doltcore/commitgraph"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
)

// buildLogFilterDocs turns the per-collection CommitFilter into a per-collection
// filter document evaluated against each walked commit's parent1 diff (touched
// semantics). A commit matches the collection when it added/removed/modified a
// document satisfying the filter. Per collection:
//   - All     -> `{}` (matches every document, i.e. any change to the collection)
//   - else    -> the OR of `{_id: {$in: [ids...]}}` (when IDs is non-empty) and
//                each $match query. With a single clause that clause is used
//                directly; with several they are wrapped in `$or`.
//
// $match queries are NOT resolved at HEAD: each is applied per commit, so a
// commit is included when it touched a document matching the query (with the
// usual pre/post-image rule for modifications). Routing through the registered
// find() matcher gives full _id equality (coercion, ObjectId, document _ids)
// and full query-operator support for free.
func buildLogFilterDocs(filters map[string]backends.CommitFilter) (map[string]*types.Document, error) {
	if len(filters) == 0 {
		return nil, nil
	}

	out := make(map[string]*types.Document, len(filters))
	for coll, cf := range filters {
		if cf.All {
			out[coll] = must.NotFail(types.NewDocument())
			continue
		}

		var clauses []any
		if len(cf.IDs) > 0 {
			arr, err := types.NewArray(cf.IDs...)
			if err != nil {
				return nil, err
			}
			inDoc, err := types.NewDocument("$in", arr)
			if err != nil {
				return nil, err
			}
			idDoc, err := types.NewDocument("_id", inDoc)
			if err != nil {
				return nil, err
			}
			clauses = append(clauses, idDoc)
		}
		for _, q := range cf.Queries {
			clauses = append(clauses, q)
		}

		switch len(clauses) {
		case 0:
			// No IDs and no queries: match nothing.
			emptyArr := must.NotFail(types.NewArray())
			out[coll] = must.NotFail(types.NewDocument("_id", must.NotFail(types.NewDocument("$in", emptyArr))))
		case 1:
			out[coll] = clauses[0].(*types.Document)
		default:
			orArr, err := types.NewArray(clauses...)
			if err != nil {
				return nil, err
			}
			out[coll] = must.NotFail(types.NewDocument("$or", orArr))
		}
	}
	return out, nil
}

// commitTouchesFilter reports whether commit ci added, removed, or modified
// (versus its first parent) a document matching the per-collection filter in
// any listed collection (OR). For the log's _id filters the predicate reduces
// to "did this commit touch one of the requested documents."
func commitTouchesFilter(
	ctx context.Context,
	db *dbState,
	ci *commitgraph.CommitInfo,
	filters map[string]*types.Document,
) (bool, error) {
	commitAM, err := amFromCommitHash(ctx, db, ci.Hash.String())
	if err != nil {
		return false, err
	}

	var parentAM prolly.AddressMap
	if len(ci.Parents) > 0 {
		parentAM, err = amFromCommitHash(ctx, db, ci.Parents[0].String())
	} else {
		parentAM, err = prolly.NewEmptyAddressMap(db.ns)
	}
	if err != nil {
		return false, err
	}

	for name, filter := range filters {
		aHash, _ := parentAM.Get(ctx, name)
		bHash, _ := commitAM.Get(ctx, name)
		if aHash == bHash {
			// Collection unchanged in this commit (content-addressed equality).
			continue
		}

		aMap, mErr := collectionMapFromAM(ctx, db, parentAM, name)
		if mErr != nil {
			return false, mErr
		}
		bMap, mErr := collectionMapFromAM(ctx, db, commitAM, name)
		if mErr != nil {
			return false, mErr
		}

		matched, mErr := anyChangedDocMatches(ctx, db.ns, aMap, bMap, filter)
		if mErr != nil {
			return false, mErr
		}
		if matched {
			return true, nil
		}
	}

	return false, nil
}

// anyChangedDocMatches walks the two collection maps in key order and returns
// true as soon as a document that changed between them matches the filter. It
// mirrors diffCollectionMaps' merge walk but short-circuits and decodes only
// the documents it needs to test.
func anyChangedDocMatches(
	ctx context.Context,
	ns tree.NodeStore,
	aMap, bMap prolly.Map,
	filter *types.Document,
) (bool, error) {
	useChanged := matchFilterUsesChanged(filter)
	matched := false
	err := forEachCollectionChange(ctx, aMap, bMap, func(c collChange) (bool, error) {
		docA, docB, dErr := decodeChange(ctx, ns, c)
		if dErr != nil {
			return false, dErr
		}
		ok, mErr := docsMatchFilter(docA, docB, filter, useChanged)
		if mErr != nil {
			return false, mErr
		}
		if ok {
			matched = true
			return true, nil // stop at first match
		}
		return false, nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

// scopedCollectionDiff is diffCollectionMaps restricted to documents matching
// the filter. It is the doc-level diff the filtered stat/patch path uses
// instead of the whole-collection diff, so stat/patch report only the matched
// documents. Added matches on the post-image, removed on the pre-image,
// modified if either image matches.
func scopedCollectionDiff(
	ctx context.Context,
	ns tree.NodeStore,
	aMap, bMap prolly.Map,
	filter *types.Document,
) (added []*types.Document, removed []*types.Document, modified []backends.ModifiedDoc, err error) {
	useChanged := matchFilterUsesChanged(filter)

	err = forEachCollectionChange(ctx, aMap, bMap, func(c collChange) (bool, error) {
		docA, docB, dErr := decodeChange(ctx, ns, c)
		if dErr != nil {
			return false, dErr
		}
		ok, mErr := docsMatchFilter(docA, docB, filter, useChanged)
		if mErr != nil {
			return false, mErr
		}
		if !ok {
			return false, nil
		}

		switch c.kind {
		case collAdded:
			added = append(added, docB)
		case collRemoved:
			removed = append(removed, docA)
		case collModified:
			id, idErr := docB.Get("_id")
			if idErr != nil {
				return false, idErr
			}
			fieldDiffs, diffErr := diffDocumentPaths("$", docA, docB, true)
			if diffErr != nil {
				return false, diffErr
			}
			if len(fieldDiffs) > 0 {
				modified = append(modified, backends.ModifiedDoc{ID: id, Diff: fieldDiffs})
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return added, removed, modified, nil
}
