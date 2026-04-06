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
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/docudolt/internal/backends"
	"github.com/dolthub/docudolt/internal/types"
	"github.com/dolthub/docudolt/internal/util/iterator"
)

// rootishIsReadOnly reports whether a rootish is read-only (commit hash or ancestor expression).
// Branch names and tag names are writable (not read-only).
//
// Dolt commit hashes are exactly 32 lowercase base32 characters (0-9a-v). Only
// full-length hashes are detected here; abbreviated forms are indistinguishable
// from branch names at parse time and are resolved at runtime by the backend.
func rootishIsReadOnly(rootish string) bool {
	// Ancestor expression: <branch>~<N>
	if strings.Contains(rootish, "~") {
		return true
	}

	// Dolt commit hash: exactly 32 lowercase base32 characters (0-9a-v).
	if len(rootish) != 32 {
		return false
	}
	for _, c := range rootish {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'v')) {
			return false
		}
	}
	return true
}

// amFromRootish resolves a rootish string to a collections AddressMap.
//
// Resolution order:
//  1. Dolt commit hash (32 base32 chars 0-9a-v): load AM from that commit directly.
//  2. Relative ancestor expression (<branch>~<N>): resolve branch HEAD, walk N first-parents.
//  3. Branch name: resolve via refs/heads/<rootish>, load AM from that branch's HEAD commit.
//  4. Tag name: resolve via refs/tags/<rootish>, load AM from the tagged commit.
//
// Non-main branch reads always reflect the committed HEAD of that branch, not its
// working set (multi-branch working sets are not yet implemented).
func amFromRootish(ctx context.Context, state *dbState, rootish string) (prolly.AddressMap, error) {
	// Case 1: commit hash (exactly 32 base32 chars 0-9a-v).
	if _, ok := hash.MaybeParse(rootish); ok && len(rootish) == 32 {
		return amFromCommitHash(ctx, state, rootish)
	}

	// Case 2: relative ancestor expression <branch>~<N>.
	if strings.Contains(rootish, "~") {
		return amFromAncestorExpr(ctx, state, rootish)
	}

	// Case 3: branch name — try refs/heads/<rootish>.
	branchDS, err := state.doltDB.GetDataset(ctx, "refs/heads/"+rootish)
	if err == nil && branchDS.HasHead() {
		branchHead, ok := branchDS.MaybeHeadAddr()
		if !ok {
			return prolly.AddressMap{}, fmt.Errorf("rootish %q: branch has no head address", rootish)
		}
		return amFromCommitHash(ctx, state, branchHead.String())
	}

	// Case 4: tag name — try refs/tags/<rootish>.
	tagDS, err := state.doltDB.GetDataset(ctx, "refs/tags/"+rootish)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("rootish %q: not a commit hash, branch, or tag: %w", rootish, err)
	}
	if !tagDS.HasHead() {
		return prolly.AddressMap{}, fmt.Errorf("rootish %q: not found as branch or tag", rootish)
	}
	tagHead, ok := tagDS.MaybeHeadAddr()
	if !ok {
		return prolly.AddressMap{}, fmt.Errorf("rootish %q: tag has no head address", rootish)
	}
	return amFromCommitHash(ctx, state, tagHead.String())
}

// amFromAncestorExpr resolves a relative ancestor expression like "main~3" to a
// collections AddressMap.
//
// It resolves the branch portion to its HEAD commit via refs/heads/<branch>, then
// walks N first-parents up the commit DAG using the Dolt parent-chain API.
// ~0 returns the branch HEAD itself.
func amFromAncestorExpr(ctx context.Context, state *dbState, rootish string) (prolly.AddressMap, error) {
	idx := strings.LastIndex(rootish, "~")
	branch := rootish[:idx]
	nStr := rootish[idx+1:]

	n, err := strconv.Atoi(nStr)
	if err != nil || n < 0 {
		return prolly.AddressMap{}, fmt.Errorf("rootish %q: invalid ancestor count %q", rootish, nStr)
	}

	// Resolve branch to HEAD commit hash.
	branchDS, dsErr := state.doltDB.GetDataset(ctx, "refs/heads/"+branch)
	if dsErr != nil {
		return prolly.AddressMap{}, fmt.Errorf("rootish %q: resolving branch %q: %w", rootish, branch, dsErr)
	}
	if !branchDS.HasHead() {
		return prolly.AddressMap{}, fmt.Errorf("rootish %q: branch %q has no commits", rootish, branch)
	}
	currentHash, ok := branchDS.MaybeHeadAddr()
	if !ok {
		return prolly.AddressMap{}, fmt.Errorf("rootish %q: branch %q has no head address", rootish, branch)
	}

	// Walk N first-parents up the commit DAG.
	for i := 0; i < n; i++ {
		commit, loadErr := datas.LoadCommitAddr(ctx, state.vs, currentHash)
		if loadErr != nil {
			return prolly.AddressMap{}, fmt.Errorf("rootish %q: loading commit at depth %d: %w", rootish, i, loadErr)
		}
		parentAddrs, parErr := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
		if parErr != nil {
			return prolly.AddressMap{}, fmt.Errorf("rootish %q: reading parents at depth %d: %w", rootish, i, parErr)
		}
		if len(parentAddrs) == 0 {
			return prolly.AddressMap{}, fmt.Errorf("rootish %q: commit at depth %d has no parent (only %d ancestors exist)", rootish, i, i)
		}
		currentHash = parentAddrs[0]
	}

	return amFromCommitHash(ctx, state, currentHash.String())
}

// resolveRootishToCommitHash resolves any rootish expression to the Dolt commit hash
// it points to. Resolution order mirrors amFromRootish:
//  1. Bare 32-char commit hash — parsed and returned directly.
//  2. Ancestor expression <branch>~<N> — branch HEAD resolved, then N first-parents walked.
//  3. Branch name — resolved via refs/heads/<rootish>.
//  4. Tag name — resolved via refs/tags/<rootish>.
//
// This is used for branch creation (DocuDoltBranch) which needs the commit hash, not the AM.
func resolveRootishToCommitHash(ctx context.Context, state *dbState, rootish string) (hash.Hash, error) {
	// Case 1: bare commit hash.
	if h, ok := hash.MaybeParse(rootish); ok && len(rootish) == 32 {
		return h, nil
	}

	// Case 2: ancestor expression <branch>~<N>.
	if idx := strings.LastIndex(rootish, "~"); idx >= 0 {
		branch := rootish[:idx]
		nStr := rootish[idx+1:]
		n, err := strconv.Atoi(nStr)
		if err != nil || n < 0 {
			return hash.Hash{}, fmt.Errorf("rootish %q: invalid ancestor count %q", rootish, nStr)
		}
		branchDS, err := state.doltDB.GetDataset(ctx, "refs/heads/"+branch)
		if err != nil {
			return hash.Hash{}, fmt.Errorf("rootish %q: resolving branch %q: %w", rootish, branch, err)
		}
		if !branchDS.HasHead() {
			return hash.Hash{}, fmt.Errorf("rootish %q: branch %q has no commits", rootish, branch)
		}
		currentHash, ok := branchDS.MaybeHeadAddr()
		if !ok {
			return hash.Hash{}, fmt.Errorf("rootish %q: branch %q has no head address", rootish, branch)
		}
		for i := 0; i < n; i++ {
			commit, loadErr := datas.LoadCommitAddr(ctx, state.vs, currentHash)
			if loadErr != nil {
				return hash.Hash{}, fmt.Errorf("rootish %q: loading commit at depth %d: %w", rootish, i, loadErr)
			}
			parentAddrs, parErr := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
			if parErr != nil {
				return hash.Hash{}, fmt.Errorf("rootish %q: reading parents at depth %d: %w", rootish, i, parErr)
			}
			if len(parentAddrs) == 0 {
				return hash.Hash{}, fmt.Errorf("rootish %q: commit at depth %d has no parent (only %d ancestors exist)", rootish, i, i)
			}
			currentHash = parentAddrs[0]
		}
		return currentHash, nil
	}

	// Case 3: branch name.
	branchDS, err := state.doltDB.GetDataset(ctx, "refs/heads/"+rootish)
	if err == nil && branchDS.HasHead() {
		if h, ok := branchDS.MaybeHeadAddr(); ok {
			return h, nil
		}
	}

	// Case 4: tag name.
	tagDS, tagErr := state.doltDB.GetDataset(ctx, "refs/tags/"+rootish)
	if tagErr == nil && tagDS.HasHead() {
		if h, ok := tagDS.MaybeHeadAddr(); ok {
			return h, nil
		}
	}

	return hash.Hash{}, fmt.Errorf("rootish %q: not found as commit hash, branch, or tag", rootish)
}

// amFromHEADExpr resolves a "HEAD" or "HEAD~N" rootish to a collections AddressMap.
//
// HEAD resolves to the committed tip of connRootish — the connection's own branch or
// snapshot, not necessarily main. HEAD~N walks N first-parents above that commit.
// HEAD~0 is equivalent to HEAD.
func amFromHEADExpr(ctx context.Context, state *dbState, connRootish, rootish string) (prolly.AddressMap, error) {
	currentHash, err := resolveRootishToCommitHash(ctx, state, connRootish)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("HEAD: resolving connection rootish %q: %w", connRootish, err)
	}

	n := 0
	if strings.HasPrefix(rootish, "HEAD~") {
		nStr := rootish[5:]
		n, err = strconv.Atoi(nStr)
		if err != nil || n < 0 {
			return prolly.AddressMap{}, fmt.Errorf("rootish %q: invalid ancestor count %q", rootish, nStr)
		}
	}

	for i := 0; i < n; i++ {
		commit, loadErr := datas.LoadCommitAddr(ctx, state.vs, currentHash)
		if loadErr != nil {
			return prolly.AddressMap{}, fmt.Errorf("rootish %q: loading commit at depth %d: %w", rootish, i, loadErr)
		}
		parentAddrs, parErr := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
		if parErr != nil {
			return prolly.AddressMap{}, fmt.Errorf("rootish %q: reading parents at depth %d: %w", rootish, i, parErr)
		}
		if len(parentAddrs) == 0 {
			return prolly.AddressMap{}, fmt.Errorf("rootish %q: commit at depth %d has no parent (only %d ancestors exist)", rootish, i, i)
		}
		currentHash = parentAddrs[0]
	}

	return amFromCommitHash(ctx, state, currentHash.String())
}

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
			diffs = append(diffs, backends.FieldDiff{Type: "removed", Path: path, From: aVal})
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
			diffs = append(diffs, backends.FieldDiff{Type: "added", Path: path, To: bVal})
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

	return []backends.FieldDiff{{Type: "modified", Path: path, From: aVal, To: bVal}}, nil
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

			diffs = append(diffs, backends.FieldDiff{Type: "added", Path: elemPath, To: bVal})

		case i >= lenB:
			aVal, err := arrA.Get(i)
			if err != nil {
				return nil, fmt.Errorf("reading array a[%d]: %w", i, err)
			}

			diffs = append(diffs, backends.FieldDiff{Type: "removed", Path: elemPath, From: aVal})

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
