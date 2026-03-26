// Copyright 2024 Dolt Inc.
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
	"fmt"
	"io"
	"sort"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
)

// amFromCommitHash loads the collections AddressMap from a specific commit
// identified by its hash string (32-char base32 dolt hash).
//
// It reads the commit chunk, parses the RTVL (RootValue) it references,
// and returns the embedded collections AddressMap.
func amFromCommitHash(ctx context.Context, state *dbState, hashStr string) (prolly.AddressMap, error) {
	h, ok := hash.MaybeParse(hashStr)
	if !ok {
		return prolly.AddressMap{}, fmt.Errorf("invalid commit hash %q", hashStr)
	}

	chunk, err := state.cs.Get(ctx, h)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("reading chunk for commit %q: %w", hashStr, err)
	}
	if chunk.IsEmpty() {
		return prolly.AddressMap{}, fmt.Errorf("commit %q not found", hashStr)
	}

	fileID := serial.GetFileID(chunk.Data())
	if fileID != serial.CommitFileID {
		return prolly.AddressMap{}, fmt.Errorf("hash %q is not a commit (got file ID %q)", hashStr, fileID)
	}

	c, err := serial.TryGetRootAsCommit(chunk.Data(), serial.MessagePrefixSz)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing commit %q: %w", hashStr, err)
	}

	rtvlHash := hash.New(c.RootBytes())

	rtvlChunk, err := state.cs.Get(ctx, rtvlHash)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("reading RTVL for commit %q: %w", hashStr, err)
	}

	rtvlFileID := serial.GetFileID(rtvlChunk.Data())
	if rtvlFileID != serial.RootValueFileID {
		return prolly.AddressMap{}, fmt.Errorf("unexpected root value file ID %q for commit %q", rtvlFileID, hashStr)
	}

	rtvl, err := serial.TryGetRootAsRootValue(rtvlChunk.Data(), serial.MessagePrefixSz)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing RTVL for commit %q: %w", hashStr, err)
	}

	amNode, _, err := tree.NodeFromBytes(rtvl.TablesBytes())
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing collections AM for commit %q: %w", hashStr, err)
	}

	return prolly.NewAddressMap(amNode, state.ns)
}

// unionCollectionNames returns the sorted union of collection names present in
// either aAM or bAM.
func unionCollectionNames(ctx context.Context, aAM, bAM prolly.AddressMap) ([]string, error) {
	seen := make(map[string]struct{})

	if err := aAM.IterAll(ctx, func(name string, _ hash.Hash) error {
		seen[name] = struct{}{}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("iterating a AM: %w", err)
	}

	if err := bAM.IterAll(ctx, func(name string, _ hash.Hash) error {
		seen[name] = struct{}{}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("iterating b AM: %w", err)
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}

	sort.Strings(names)

	return names, nil
}

// readDocFromEntry reads a full document from a prolly.Map key+value entry.
func readDocFromEntry(ctx context.Context, ns tree.NodeStore, k, v val.Tuple) (*types.Document, error) {
	jsonHash, ok := valDesc.GetJSONAddr(0, v)
	if !ok {
		return nil, fmt.Errorf("extracting JSON hash from value tuple")
	}

	return readDocJSON(ctx, ns, jsonHash)
}

// idFromKeyTuple decodes the _id value from a prolly.Map key tuple.
func idFromKeyTuple(k val.Tuple) (any, error) {
	keyBytes, ok := keyDesc.GetBytes(0, k)
	if !ok {
		return nil, fmt.Errorf("extracting key bytes from tuple")
	}

	return decodeID(keyBytes)
}

// diffDocumentPaths computes path-based field diffs between two documents,
// producing a []backends.FieldDiff where each entry carries a JSON Path string
// (e.g. "$.field", "$.nested.field", "$.array[2]"), a type tag ("added",
// "modified", "removed"), and the old and/or new values as MongoDB-typed values.
//
// The _id field is excluded since it is reported at the ModifiedDoc level.
// Returns nil if the documents are identical.
func diffDocumentPaths(prefix string, docA, docB *types.Document) ([]backends.FieldDiff, error) {
	// Build a map of b's non-_id fields for O(1) lookup.
	bFieldMap := make(map[string]any)
	bIter := docB.Iterator()
	defer bIter.Close()

	for {
		k, v, err := bIter.Next()
		if err != nil {
			if err == iterator.ErrIteratorDone {
				break
			}

			return nil, fmt.Errorf("iterating b document: %w", err)
		}

		if k == "_id" {
			continue
		}

		bFieldMap[k] = v
	}

	var diffs []backends.FieldDiff

	// Walk a's fields.
	aIter := docA.Iterator()
	defer aIter.Close()

	aFieldsSeen := make(map[string]struct{})

	for {
		k, aVal, err := aIter.Next()
		if err != nil {
			if err == iterator.ErrIteratorDone {
				break
			}

			return nil, fmt.Errorf("iterating a document: %w", err)
		}

		if k == "_id" {
			continue
		}

		aFieldsSeen[k] = struct{}{}
		path := prefix + "." + k

		bVal, inB := bFieldMap[k]
		if !inB {
			// Field was removed.
			diffs = append(diffs, backends.FieldDiff{Type: "removed", Path: path, A: aVal})
		} else {
			// Field in both — compare values recursively.
			subdiffs, err := compareFieldPaths(path, aVal, bVal)
			if err != nil {
				return nil, err
			}

			diffs = append(diffs, subdiffs...)
		}
	}

	// Fields only in b (added).
	for k, bVal := range bFieldMap {
		if _, inA := aFieldsSeen[k]; !inA {
			path := prefix + "." + k
			diffs = append(diffs, backends.FieldDiff{Type: "added", Path: path, B: bVal})
		}
	}

	return diffs, nil
}

// compareFieldPaths compares two field values, recursing into nested documents
// and arrays. It returns a FieldDiff for each differing leaf.
func compareFieldPaths(path string, aVal, bVal any) ([]backends.FieldDiff, error) {
	// Nested documents — recurse.
	aDoc, aIsDoc := aVal.(*types.Document)
	bDoc, bIsDoc := bVal.(*types.Document)

	if aIsDoc && bIsDoc {
		return diffDocumentPaths(path, aDoc, bDoc)
	}

	// Arrays — compare element by element.
	aArr, aIsArr := aVal.(*types.Array)
	bArr, bIsArr := bVal.(*types.Array)

	if aIsArr && bIsArr {
		return diffArrayPaths(path, aArr, bArr)
	}

	// Scalars (or type mismatch between doc/array/scalar).
	if types.Compare(aVal, bVal) == types.Equal {
		return nil, nil
	}

	return []backends.FieldDiff{{Type: "modified", Path: path, A: aVal, B: bVal}}, nil
}

// diffArrayPaths compares two arrays element-by-element and returns path-based
// diffs with bracket notation (e.g. "$.scores[2]").
func diffArrayPaths(path string, arrA, arrB *types.Array) ([]backends.FieldDiff, error) {
	lenA := arrA.Len()
	lenB := arrB.Len()

	maxLen := lenA
	if lenB > maxLen {
		maxLen = lenB
	}

	var diffs []backends.FieldDiff

	for i := 0; i < maxLen; i++ {
		elemPath := fmt.Sprintf("%s[%d]", path, i)

		switch {
		case i >= lenA:
			bVal, err := arrB.Get(i)
			if err != nil {
				return nil, fmt.Errorf("reading array b[%d]: %w", i, err)
			}

			diffs = append(diffs, backends.FieldDiff{Type: "added", Path: elemPath, B: bVal})

		case i >= lenB:
			aVal, err := arrA.Get(i)
			if err != nil {
				return nil, fmt.Errorf("reading array a[%d]: %w", i, err)
			}

			diffs = append(diffs, backends.FieldDiff{Type: "removed", Path: elemPath, A: aVal})

		default:
			aVal, err := arrA.Get(i)
			if err != nil {
				return nil, fmt.Errorf("reading array a[%d]: %w", i, err)
			}

			bVal, err := arrB.Get(i)
			if err != nil {
				return nil, fmt.Errorf("reading array b[%d]: %w", i, err)
			}

			subdiffs, err := compareFieldPaths(elemPath, aVal, bVal)
			if err != nil {
				return nil, err
			}

			diffs = append(diffs, subdiffs...)
		}
	}

	return diffs, nil
}

// diffCollectionMaps computes the document-level diff between two prolly.Maps
// (one per collection). Both maps use the same key/value descriptors as
// the rest of the dolt backend.
//
// It iterates both sorted maps in parallel (merge-join) to find:
//   - Documents only in aMap → removed
//   - Documents only in bMap → added
//   - Documents in both with different values → modified (path-based field diff)
//
// Documents present in both with identical values are not included in any list.
// For modified documents, the prolly-map content-hash comparison detects changes
// without deserializing unchanged documents.
func diffCollectionMaps(
	ctx context.Context,
	ns tree.NodeStore,
	aMap, bMap prolly.Map,
) (added []*types.Document, removed []*types.Document, modified []backends.ModifiedDoc, err error) {
	iterA, err := aMap.IterAll(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("iterating a map: %w", err)
	}

	iterB, err := bMap.IterAll(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("iterating b map: %w", err)
	}

	kA, vA, errA := iterA.Next(ctx)
	kB, vB, errB := iterB.Next(ctx)

	for {
		doneA := errA == io.EOF
		doneB := errB == io.EOF

		if doneA && doneB {
			break
		}

		if errA != nil && !doneA {
			return nil, nil, nil, fmt.Errorf("iterating a map: %w", errA)
		}

		if errB != nil && !doneB {
			return nil, nil, nil, fmt.Errorf("iterating b map: %w", errB)
		}

		switch {
		case doneA:
			// All remaining B entries are new (added).
			doc, readErr := readDocFromEntry(ctx, ns, kB, vB)
			if readErr != nil {
				return nil, nil, nil, readErr
			}

			added = append(added, doc)
			kB, vB, errB = iterB.Next(ctx)

		case doneB:
			// All remaining A entries have been deleted (removed).
			doc, readErr := readDocFromEntry(ctx, ns, kA, vA)
			if readErr != nil {
				return nil, nil, nil, readErr
			}

			removed = append(removed, doc)
			kA, vA, errA = iterA.Next(ctx)

		default:
			cmp := bytes.Compare(kA, kB)

			switch {
			case cmp < 0:
				// kA is not in B → removed.
				doc, readErr := readDocFromEntry(ctx, ns, kA, vA)
				if readErr != nil {
					return nil, nil, nil, readErr
				}

				removed = append(removed, doc)
				kA, vA, errA = iterA.Next(ctx)

			case cmp > 0:
				// kB is not in A → added.
				doc, readErr := readDocFromEntry(ctx, ns, kB, vB)
				if readErr != nil {
					return nil, nil, nil, readErr
				}

				added = append(added, doc)
				kB, vB, errB = iterB.Next(ctx)

			default:
				// Same key. The prolly-map value stores the JSON content hash,
				// so a byte-level comparison quickly detects any change without
				// deserializing either document.
				if !bytes.Equal(vA, vB) {
					docA, readErr := readDocFromEntry(ctx, ns, kA, vA)
					if readErr != nil {
						return nil, nil, nil, readErr
					}

					docB, readErr := readDocFromEntry(ctx, ns, kB, vB)
					if readErr != nil {
						return nil, nil, nil, readErr
					}

					id, idErr := idFromKeyTuple(kA)
					if idErr != nil {
						return nil, nil, nil, idErr
					}

					fieldDiffs, diffErr := diffDocumentPaths("$", docA, docB)
					if diffErr != nil {
						return nil, nil, nil, diffErr
					}

					if len(fieldDiffs) > 0 {
						modified = append(modified, backends.ModifiedDoc{
							ID:   id,
							Diff: fieldDiffs,
						})
					}
				}

				kA, vA, errA = iterA.Next(ctx)
				kB, vB, errB = iterB.Next(ctx)
			}
		}
	}

	return added, removed, modified, nil
}
