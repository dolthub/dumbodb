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
	"sort"
	"strconv"
	"strings"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// rootishIsReadOnly reports whether a rootish is syntactically read-only.
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

func rootishIsSnapshot(ctx context.Context, state *dbState, rootish string) bool {
	if rootishIsReadOnly(rootish) {
		return true
	}
	if rootish == defaultBranch {
		return false
	}
	tagDS, err := state.datasDB.GetDataset(ctx, "refs/tags/"+rootish)
	return err == nil && tagDS.HasHead()
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

	// Case 2: traversal expressions containing ^ or ~ (possibly chained).
	if strings.ContainsAny(rootish, "^~") {
		h, err := resolveRootishToCommitHash(ctx, state, rootish)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		return amFromCommitHash(ctx, state, h.String())
	}

	// Case 3: branch name  -- try refs/heads/<rootish>.
	branchDS, err := state.datasDB.GetDataset(ctx, "refs/heads/"+rootish)
	if err == nil && branchDS.HasHead() {
		branchHead, ok := branchDS.MaybeHeadAddr()
		if !ok {
			return prolly.AddressMap{}, fmt.Errorf("rootish %q: branch has no head address", rootish)
		}
		return amFromCommitHash(ctx, state, branchHead.String())
	}

	// Case 4: tag name  -- use tagCommitAddr to dereference through the tag
	// flatbuffer to the underlying commit hash.
	tagDS, err := state.datasDB.GetDataset(ctx, "refs/tags/"+rootish)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("rootish %q: not a commit hash, branch, or tag: %w", rootish, err)
	}
	if !tagDS.HasHead() {
		return prolly.AddressMap{}, fmt.Errorf("rootish %q: not found as branch or tag", rootish)
	}
	if h := tagCommitAddr(tagDS); !h.IsEmpty() {
		return amFromCommitHash(ctx, state, h.String())
	}
	return prolly.AddressMap{}, fmt.Errorf("rootish %q: tag has no commit address", rootish)
}

// resolveRootishToCommitHash resolves any rootish expression to the Dolt commit hash
// it points to. Resolution order mirrors amFromRootish:
//  1. Bare 32-char commit hash  -- parsed and returned directly.
//  2. Caret parent selection <ref>^N  -- selects Nth parent (1=first, 2=second, 0=self).
//  3. Ancestor expression <branch>~<N>  -- branch HEAD resolved, then N first-parents walked.
//  4. Branch name  -- resolved via refs/heads/<rootish>.
//  5. Tag name  -- resolved via refs/tags/<rootish>.
//
// This is used for branch creation (DumboDBBranch) which needs the commit hash, not the AM.
func resolveRootishToCommitHash(ctx context.Context, state *dbState, rootish string) (hash.Hash, error) {
	// Case 1: bare commit hash.
	if h, ok := hash.MaybeParse(rootish); ok && len(rootish) == 32 {
		return h, nil
	}

	// Case 2: chained traversal expressions containing ^ or ~.
	// Parse from right to left, peeling off one ^N or ~N suffix at a time,
	// matching git's behavior for chains like HEAD^2~3 or main~1^2.
	if strings.ContainsAny(rootish, "^~") {
		return resolveTraversalChain(ctx, state, rootish)
	}

	// Case 4: branch name.
	branchDS, err := state.datasDB.GetDataset(ctx, "refs/heads/"+rootish)
	if err == nil && branchDS.HasHead() {
		if h, ok := branchDS.MaybeHeadAddr(); ok {
			return h, nil
		}
	}

	// Case 5: tag name  -- use tagCommitAddr to dereference through the tag
	// flatbuffer to the underlying commit hash.
	tagDS, tagErr := state.datasDB.GetDataset(ctx, "refs/tags/"+rootish)
	if tagErr == nil && tagDS.HasHead() {
		if h := tagCommitAddr(tagDS); !h.IsEmpty() {
			return h, nil
		}
	}

	return hash.Hash{}, fmt.Errorf("rootish %q: not found as commit hash, branch, or tag", rootish)
}

// resolveTraversalChain resolves rootish expressions that contain ^ and/or ~
// operators, possibly chained (e.g. "main~1^2", "HEAD^2~3", "main^^").
//
// It peels off the rightmost operator suffix, resolves the base recursively,
// then applies the operator. This matches git's left-to-right evaluation:
// "HEAD^2~3" means: second parent of HEAD, then walk 3 first-parent ancestors.
func resolveTraversalChain(ctx context.Context, state *dbState, rootish string) (hash.Hash, error) {
	// Find the rightmost ^ or ~ operator.
	lastCaret := strings.LastIndex(rootish, "^")
	lastTilde := strings.LastIndex(rootish, "~")

	// Pick whichever is further right.
	splitIdx := lastCaret
	if lastTilde > splitIdx {
		splitIdx = lastTilde
	}

	op := rootish[splitIdx]
	ref := rootish[:splitIdx]
	nStr := rootish[splitIdx+1:]

	// Resolve the base ref recursively.
	refHash, err := resolveRootishToCommitHash(ctx, state, ref)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("rootish %q: resolving %q: %w", rootish, ref, err)
	}

	if op == '^' {
		n := 1 // bare ^ means ^1
		if nStr != "" {
			n, err = strconv.Atoi(nStr)
			if err != nil || n < 0 || n > 2 {
				return hash.Hash{}, fmt.Errorf("rootish %q: invalid caret index %q (must be 0, 1, or 2)", rootish, nStr)
			}
		}
		if n == 0 {
			return refHash, nil
		}
		commit, loadErr := datas.LoadCommitAddr(ctx, state.vs, refHash)
		if loadErr != nil {
			return hash.Hash{}, fmt.Errorf("rootish %q: loading commit: %w", rootish, loadErr)
		}
		parentAddrs, parErr := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
		if parErr != nil {
			return hash.Hash{}, fmt.Errorf("rootish %q: reading parents: %w", rootish, parErr)
		}
		if n > len(parentAddrs) {
			return hash.Hash{}, fmt.Errorf("rootish %q: commit has %d parent(s), cannot access parent %d", rootish, len(parentAddrs), n)
		}
		return parentAddrs[n-1], nil
	}

	// op == '~'
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 0 {
		return hash.Hash{}, fmt.Errorf("rootish %q: invalid ancestor count %q", rootish, nStr)
	}
	currentHash := refHash
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

// amFromHEADExpr resolves a "HEAD" or "HEAD~N" rootish to a collections AddressMap.
//
// HEAD resolves to the committed tip of connRootish  -- the connection's own branch or
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
//
// This iterates both address maps in full rather than using a structural-
// sharing diff. That is intentional: the cost is O(number of collections),
// not O(number of documents) -- callers then skip unchanged collections by
// comparing each collection's content address before diffing its documents
// (see DumboDBLog stat/patch and DumboDBDiff). prolly.AddressMap also exposes
// no public diff primitive (only IterAll/Get), so a DiffMaps-style walk is not
// available here. The document-level diffs that do scale with collection size
// use forEachCollectionChange (collection_diff.go).
func unionCollectionNames(ctx context.Context, cs *nbs.GenerationalNBS, aAM, bAM prolly.AddressMap) ([]string, error) {
	seen := make(map[string]struct{})

	collect := func(am prolly.AddressMap) error {
		return am.IterAll(ctx, func(name string, h hash.Hash) error {
			if name == reservedCatalogName {
				return nil
			}
			isView, err := isViewEntry(ctx, cs, h)
			if err != nil {
				return err
			}
			if !isView {
				seen[name] = struct{}{}
			}
			return nil
		})
	}

	if err := collect(aAM); err != nil {
		return nil, fmt.Errorf("iterating a AM: %w", err)
	}
	if err := collect(bAM); err != nil {
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
	return readDocFromValue(ctx, ns, v)
}

// diffDocumentPaths computes path-based field diffs between two documents,
// producing a []backends.FieldDiff where each entry carries a JSON Path string
// (e.g. "$.field", "$.nested.field", "$.array[2]"), a type tag ("added",
// "modified", "removed"), and the old and/or new values as MongoDB-typed values.
//
// skipID excludes every field named _id, at any depth, and is what document
// diffs want since _id is reported at the ModifiedDoc level. Callers diffing
// something other than a document (collection metadata) must pass false, or a
// key legitimately named _id is dropped from the result.
//
// Returns nil if the documents are identical.
func diffDocumentPaths(prefix string, docA, docB *types.Document, skipID bool) ([]backends.FieldDiff, error) {
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

		if skipID && k == "_id" {
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

		if skipID && k == "_id" {
			continue
		}

		aFieldsSeen[k] = struct{}{}
		path := prefix + "." + k

		bVal, inB := bFieldMap[k]
		if !inB {
			// Field was removed.
			diffs = append(diffs, backends.FieldDiff{Type: "removed", Path: path, From: aVal})
		} else {
			// Field in both  -- compare values recursively.
			subdiffs, err := compareFieldPaths(path, aVal, bVal, skipID)
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
func compareFieldPaths(path string, aVal, bVal any, skipID bool) ([]backends.FieldDiff, error) {
	// Nested documents  -- recurse.
	aDoc, aIsDoc := aVal.(*types.Document)
	bDoc, bIsDoc := bVal.(*types.Document)

	if aIsDoc && bIsDoc {
		return diffDocumentPaths(path, aDoc, bDoc, skipID)
	}

	// Arrays  -- compare element by element.
	aArr, aIsArr := aVal.(*types.Array)
	bArr, bIsArr := bVal.(*types.Array)

	if aIsArr && bIsArr {
		return diffArrayPaths(path, aArr, bArr, skipID)
	}

	// Scalars (or type mismatch between doc/array/scalar).
	if types.Compare(aVal, bVal) == types.Equal {
		return nil, nil
	}

	return []backends.FieldDiff{{Type: "modified", Path: path, From: aVal, To: bVal}}, nil
}

// diffArrayPaths compares two arrays element-by-element and returns path-based
// diffs with bracket notation (e.g. "$.scores[2]").
func diffArrayPaths(path string, arrA, arrB *types.Array, skipID bool) ([]backends.FieldDiff, error) {
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

			subdiffs, err := compareFieldPaths(elemPath, aVal, bVal, skipID)
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
//   - Documents only in aMap -> removed
//   - Documents only in bMap -> added
//   - Documents in both with different values -> modified (path-based field diff)
//
// Documents present in both with identical values are not included in any list.
// For modified documents, the prolly-map content-hash comparison detects changes
// without deserializing unchanged documents.
func diffCollectionMaps(
	ctx context.Context,
	ns tree.NodeStore,
	aMap, bMap prolly.Map,
) (added []*types.Document, removed []*types.Document, modified []backends.ModifiedDoc, err error) {
	// Walk only the changed documents via prolly's structural-sharing diff.
	// Diffs arrive in key order, matching the previous merge-walk output order.
	err = forEachCollectionChange(ctx, aMap, bMap, func(c collChange) (bool, error) {
		switch c.kind {
		case collAdded:
			doc, readErr := readDocFromEntry(ctx, ns, c.key, c.to)
			if readErr != nil {
				return false, readErr
			}
			added = append(added, doc)

		case collRemoved:
			doc, readErr := readDocFromEntry(ctx, ns, c.key, c.from)
			if readErr != nil {
				return false, readErr
			}
			removed = append(removed, doc)

		case collModified:
			docA, readErr := readDocFromEntry(ctx, ns, c.key, c.from)
			if readErr != nil {
				return false, readErr
			}
			docB, readErr := readDocFromEntry(ctx, ns, c.key, c.to)
			if readErr != nil {
				return false, readErr
			}
			id, idErr := docA.Get("_id")
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

// countCollectionMapDiffs counts the document-level differences between two
// prolly.Maps without deserializing documents. It parallels diffCollectionMaps
// but skips the types.Document construction  -- callers that need only counts
// (e.g. DumboDBStatus) avoid the per-document decode cost this way.
//
// The prolly-map value for each document encodes its content hash, so a
// byte-level comparison of values at the same key reliably detects changes.
func countCollectionMapDiffs(
	ctx context.Context,
	aMap, bMap prolly.Map,
) (added, modified, deleted int, err error) {
	// Counting needs no document decode: tally per change type from the
	// structural-sharing diff (which visits only changed entries).
	err = forEachCollectionChange(ctx, aMap, bMap, func(c collChange) (bool, error) {
		switch c.kind {
		case collAdded:
			added++
		case collRemoved:
			deleted++
		case collModified:
			modified++
		}
		return false, nil
	})
	if err != nil {
		return 0, 0, 0, err
	}
	return added, modified, deleted, nil
}

type collectionChange struct {
	Name        string
	Status      string
	AMap, BMap  prolly.Map
	IdxAdded    []backends.IndexInfo
	IdxModified []backends.IndexChange
	IdxRemoved  []backends.IndexInfo
	MetaDiff    []backends.FieldDiff
}

func (c collectionChange) surfacesWithoutDocChange() bool {
	return c.Status != "modified" ||
		len(c.IdxAdded) > 0 || len(c.IdxModified) > 0 || len(c.IdxRemoved) > 0 ||
		len(c.MetaDiff) > 0
}

func statusOf(aHash, bHash hash.Hash) string {
	switch {
	case aHash.IsEmpty() && !bHash.IsEmpty():
		return "added"
	case !aHash.IsEmpty() && bHash.IsEmpty():
		return "deleted"
	default:
		return "modified"
	}
}

func eachCollectionChange(ctx context.Context, state *dbState, aAM, bAM prolly.AddressMap, fn func(collectionChange) error) error {
	names, err := unionCollectionNames(ctx, state.cs, aAM, bAM)
	if err != nil {
		return err
	}

	aCatHash, err := aAM.Get(ctx, reservedCatalogName)
	if err != nil {
		return err
	}
	bCatHash, err := bAM.Get(ctx, reservedCatalogName)
	if err != nil {
		return err
	}
	catalogChanged := aCatHash != bCatHash
	var aCat, bCat prolly.Map
	if catalogChanged {
		if aCat, err = catalogMapFromAM(ctx, state, aAM); err != nil {
			return err
		}
		if bCat, err = catalogMapFromAM(ctx, state, bAM); err != nil {
			return err
		}
	}

	for _, name := range names {
		aHash, err := aAM.Get(ctx, name)
		if err != nil {
			return err
		}
		bHash, err := bAM.Get(ctx, name)
		if err != nil {
			return err
		}

		var metaDiff []backends.FieldDiff
		if catalogChanged {
			if metaDiff, err = collectionMetadataDiff(ctx, state, aCat, bCat, name); err != nil {
				return err
			}
		}

		if aHash == bHash {
			if len(metaDiff) == 0 {
				continue
			}
			m, err := collectionMapFromAM(ctx, state, aAM, name)
			if err != nil {
				return err
			}
			if err := fn(collectionChange{
				Name: name, Status: "modified", AMap: m, BMap: m,
				MetaDiff: metaDiff,
			}); err != nil {
				return err
			}
			continue
		}

		idxAdded, idxModified, idxRemoved, err := computeIndexChanges(ctx, state, aHash, bHash)
		if err != nil {
			return err
		}
		aMap, err := collectionMapFromAM(ctx, state, aAM, name)
		if err != nil {
			return err
		}
		bMap, err := collectionMapFromAM(ctx, state, bAM, name)
		if err != nil {
			return err
		}
		if err := fn(collectionChange{
			Name: name, Status: statusOf(aHash, bHash), AMap: aMap, BMap: bMap,
			IdxAdded: idxAdded, IdxModified: idxModified, IdxRemoved: idxRemoved,
			MetaDiff: metaDiff,
		}); err != nil {
			return err
		}
	}
	return nil
}

func tableStatusFrom(c collectionChange, added, modified, deleted int) backends.TableStatus {
	return backends.TableStatus{
		Name:            c.Name,
		Status:          c.Status,
		Added:           added,
		Modified:        modified,
		Deleted:         deleted,
		AddedIndexes:    indexNamesOf(c.IdxAdded),
		ModifiedIndexes: indexChangeNamesOf(c.IdxModified),
		RemovedIndexes:  indexNamesOf(c.IdxRemoved),
		MetadataDiff:    c.MetaDiff,
	}
}

func collectionDiffFrom(c collectionChange, added, removed []*types.Document, modified []backends.ModifiedDoc) backends.CollectionDiff {
	return backends.CollectionDiff{
		Name:            c.Name,
		Status:          c.Status,
		Added:           added,
		Removed:         removed,
		Modified:        modified,
		AddedIndexes:    c.IdxAdded,
		ModifiedIndexes: c.IdxModified,
		RemovedIndexes:  c.IdxRemoved,
		MetadataDiff:    c.MetaDiff,
	}
}

// metadataViewOf returns nil when the collection has no validator.
func metadataViewOf(m *collMeta) *backends.CollectionMetadata {
	if m == nil || m.Validator == nil {
		return nil
	}
	return collMetaToMetadata(m)
}

// Wire paths for collection metadata field diffs, rooted at the collection
// spec the way document paths are rooted at the document. A validator change
// reports the leaves beneath validatorPath, so the paths reach into the
// validator (e.g. "$.validator.$jsonSchema.properties.email.pattern").
const (
	validatorPath        = "$.validator"
	validationLevelPath  = "$.validationLevel"
	validationActionPath = "$.validationAction"
)

// collectionMetadataDiff returns the field diffs between the two sides'
// user-facing metadata, empty when the metadata is unchanged. A side with no
// validator has no metadata view at all, so the other side's fields all report
// as added or removed.
func collectionMetadataDiff(ctx context.Context, state *dbState, aCat, bCat prolly.Map, name string) ([]backends.FieldDiff, error) {
	aMeta, err := readCollMetaFromCatalog(ctx, state, aCat, name)
	if err != nil {
		return nil, err
	}
	bMeta, err := readCollMetaFromCatalog(ctx, state, bCat, name)
	if err != nil {
		return nil, err
	}
	aView := metadataViewOf(aMeta)
	bView := metadataViewOf(bMeta)

	switch {
	case aView == nil && bView == nil:
		return nil, nil
	case aView == nil:
		return metadataSideDiffs("added", bView), nil
	case bView == nil:
		return metadataSideDiffs("removed", aView), nil
	}

	var diffs []backends.FieldDiff

	equalValidators, err := validatorsEqual(aView.Validator, bView.Validator)
	if err != nil {
		return nil, err
	}
	if !equalValidators {
		// skipID is false: a validator key named _id is a real key, not a
		// document identifier ($jsonSchema.properties._id, or a bare {_id: ...}
		// query expression).
		validatorDiffs, err := diffDocumentPaths(validatorPath, aView.Validator, bView.Validator, false)
		if err != nil {
			return nil, err
		}
		if len(validatorDiffs) == 0 {
			// Canonical BSON differs but every leaf compares equal, e.g. a
			// numeric width change. Report the validator whole rather than
			// drop a change the catalog did record.
			validatorDiffs = []backends.FieldDiff{{
				Type: "modified", Path: validatorPath,
				From: aView.Validator, To: bView.Validator,
			}}
		}
		diffs = append(diffs, validatorDiffs...)
	}
	if aView.ValidationLevel != bView.ValidationLevel {
		diffs = append(diffs, backends.FieldDiff{
			Type: "modified", Path: validationLevelPath,
			From: aView.ValidationLevel, To: bView.ValidationLevel,
		})
	}
	if aView.ValidationAction != bView.ValidationAction {
		diffs = append(diffs, backends.FieldDiff{
			Type: "modified", Path: validationActionPath,
			From: aView.ValidationAction, To: bView.ValidationAction,
		})
	}

	return diffs, nil
}

// metadataSideDiffs reports every field of the one present side, for a
// collection whose validator appeared or disappeared outright. diffType is
// "added" when the present side is "b", "removed" when it is "a".
func metadataSideDiffs(diffType string, m *backends.CollectionMetadata) []backends.FieldDiff {
	entry := func(path string, value any) backends.FieldDiff {
		fd := backends.FieldDiff{Type: diffType, Path: path}
		if diffType == "removed" {
			fd.From = value
		} else {
			fd.To = value
		}
		return fd
	}
	return []backends.FieldDiff{
		entry(validatorPath, m.Validator),
		entry(validationLevelPath, m.ValidationLevel),
		entry(validationActionPath, m.ValidationAction),
	}
}

// validatorsEqual compares two validators by canonical BSON, so key order does
// not register as a change.
func validatorsEqual(a, b *types.Document) (bool, error) {
	aBytes, err := docToBSON(a)
	if err != nil {
		return false, err
	}
	bBytes, err := docToBSON(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(aBytes, bBytes), nil
}
