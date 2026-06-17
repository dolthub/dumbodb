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
	"bytes"
	"context"
	"io"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"

	"github.com/dolthub/dolt/go/libraries/doltcore/commitgraph"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
)

// idFilterDocs converts the simple log filter (collection name -> list of _id
// values) into a per-collection filter document. A non-empty id list becomes
// `{_id: {$in: [...]}}`; an empty list is the whole-collection wildcard and
// becomes `{}` (matches every document). Routing through the registered find()
// matcher gives MongoDB _id equality for free -- numeric cross-type coercion
// (int32/int64/double), ObjectId, and document _ids.
func idFilterDocs(filters map[string][]any) (map[string]*types.Document, error) {
	if len(filters) == 0 {
		return nil, nil
	}
	out := make(map[string]*types.Document, len(filters))
	for coll, ids := range filters {
		if len(ids) == 0 {
			// Whole-collection wildcard: match any document in the collection.
			out[coll] = must.NotFail(types.NewDocument())
			continue
		}
		arr, err := types.NewArray(ids...)
		if err != nil {
			return nil, err
		}
		inDoc, err := types.NewDocument("$in", arr)
		if err != nil {
			return nil, err
		}
		filterDoc, err := types.NewDocument("_id", inDoc)
		if err != nil {
			return nil, err
		}
		out[coll] = filterDoc
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
	iterA, err := aMap.IterAll(ctx)
	if err != nil {
		return false, err
	}
	iterB, err := bMap.IterAll(ctx)
	if err != nil {
		return false, err
	}

	kA, vA, errA := iterA.Next(ctx)
	kB, vB, errB := iterB.Next(ctx)

	testEntry := func(k, v val.Tuple) (bool, error) {
		doc, readErr := readDocFromEntry(ctx, ns, k, v)
		if readErr != nil {
			return false, readErr
		}
		return backends.MatchPartialFilter(doc, filter)
	}

	for {
		doneA := errA == io.EOF
		doneB := errB == io.EOF
		if doneA && doneB {
			break
		}
		if errA != nil && !doneA {
			return false, errA
		}
		if errB != nil && !doneB {
			return false, errB
		}

		switch {
		case doneA:
			if ok, tErr := testEntry(kB, vB); tErr != nil || ok {
				return ok, tErr
			}
			kB, vB, errB = iterB.Next(ctx)

		case doneB:
			if ok, tErr := testEntry(kA, vA); tErr != nil || ok {
				return ok, tErr
			}
			kA, vA, errA = iterA.Next(ctx)

		default:
			switch cmp := bytes.Compare(kA, kB); {
			case cmp < 0:
				if ok, tErr := testEntry(kA, vA); tErr != nil || ok {
					return ok, tErr
				}
				kA, vA, errA = iterA.Next(ctx)

			case cmp > 0:
				if ok, tErr := testEntry(kB, vB); tErr != nil || ok {
					return ok, tErr
				}
				kB, vB, errB = iterB.Next(ctx)

			default:
				// Same key; modified when the content hashes differ. A modified
				// document matches if either image matches the filter.
				if !bytes.Equal(vA, vB) {
					if ok, tErr := testEntry(kA, vA); tErr != nil || ok {
						return ok, tErr
					}
					if ok, tErr := testEntry(kB, vB); tErr != nil || ok {
						return ok, tErr
					}
				}
				kA, vA, errA = iterA.Next(ctx)
				kB, vB, errB = iterB.Next(ctx)
			}
		}
	}

	return false, nil
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
	iterA, err := aMap.IterAll(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	iterB, err := bMap.IterAll(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	kA, vA, errA := iterA.Next(ctx)
	kB, vB, errB := iterB.Next(ctx)

	readMatch := func(k, v val.Tuple) (*types.Document, bool, error) {
		doc, rErr := readDocFromEntry(ctx, ns, k, v)
		if rErr != nil {
			return nil, false, rErr
		}
		ok, mErr := backends.MatchPartialFilter(doc, filter)
		return doc, ok, mErr
	}

	for {
		doneA := errA == io.EOF
		doneB := errB == io.EOF
		if doneA && doneB {
			break
		}
		if errA != nil && !doneA {
			return nil, nil, nil, errA
		}
		if errB != nil && !doneB {
			return nil, nil, nil, errB
		}

		switch {
		case doneA:
			doc, ok, mErr := readMatch(kB, vB)
			if mErr != nil {
				return nil, nil, nil, mErr
			}
			if ok {
				added = append(added, doc)
			}
			kB, vB, errB = iterB.Next(ctx)

		case doneB:
			doc, ok, mErr := readMatch(kA, vA)
			if mErr != nil {
				return nil, nil, nil, mErr
			}
			if ok {
				removed = append(removed, doc)
			}
			kA, vA, errA = iterA.Next(ctx)

		default:
			switch cmp := bytes.Compare(kA, kB); {
			case cmp < 0:
				doc, ok, mErr := readMatch(kA, vA)
				if mErr != nil {
					return nil, nil, nil, mErr
				}
				if ok {
					removed = append(removed, doc)
				}
				kA, vA, errA = iterA.Next(ctx)

			case cmp > 0:
				doc, ok, mErr := readMatch(kB, vB)
				if mErr != nil {
					return nil, nil, nil, mErr
				}
				if ok {
					added = append(added, doc)
				}
				kB, vB, errB = iterB.Next(ctx)

			default:
				if !bytes.Equal(vA, vB) {
					docA, matchA, mErr := readMatch(kA, vA)
					if mErr != nil {
						return nil, nil, nil, mErr
					}
					docB, matchB, mErr := readMatch(kB, vB)
					if mErr != nil {
						return nil, nil, nil, mErr
					}
					if matchA || matchB {
						id, idErr := docB.Get("_id")
						if idErr != nil {
							return nil, nil, nil, idErr
						}
						fieldDiffs, diffErr := diffDocumentPaths("$", docA, docB)
						if diffErr != nil {
							return nil, nil, nil, diffErr
						}
						if len(fieldDiffs) > 0 {
							modified = append(modified, backends.ModifiedDoc{ID: id, Diff: fieldDiffs})
						}
					}
				}
				kA, vA, errA = iterA.Next(ctx)
				kB, vB, errB = iterB.Next(ctx)
			}
		}
	}

	return added, removed, modified, nil
}
