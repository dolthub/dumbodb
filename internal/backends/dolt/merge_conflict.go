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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/zeebo/xxh3"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
)

// mergeInProgress holds the state of a merge or cherry-pick that could not be
// automatically completed due to document-level conflicts. It persists in dbState
// until the user resolves all conflicts.
//
// When isCherryPick is false: fromBranch and fromHash identify the source branch;
// the final commit has two parents (intoHash and fromHash).
//
// When isCherryPick is true: pickHash is the cherry-picked commit; intoHash is
// the current branch HEAD; the final commit has one parent (intoHash).
// fromBranch is unused in cherry-pick mode; pickHash stores the cherry-pick source.
// originalMsg holds the original commit message of the cherry-picked commit for
// the default annotation.
type mergeInProgress struct {
	fromBranch string
	intoBranch string
	premergeAM prolly.AddressMap // ours branch AM before the operation started (used to abort)
	fromHash   hash.Hash         // fromBranch HEAD hash at merge time (merge commit parent 2)
	intoHash   hash.Hash         // intoBranch HEAD hash at merge/cherry-pick time (commit parent 1)
	// conflicts: collection name -> ordered list of conflict entries.
	// Entries are never removed; resolved ones have resolved==true.
	conflicts map[string][]*conflictEntry
	// viewConflicts: view name -> the view-definition conflict for that name.
	// A view diverged on both branches; resolved ones have resolved==true.
	viewConflicts map[string]*viewConflictEntry
	// resolvedAM is the working AddressMap being built as conflicts are resolved.
	// It starts as the partial merged AM (keeping "ours" for conflicting docs) and
	// is updated as each conflict is resolved.
	resolvedAM prolly.AddressMap

	// Cherry-pick specific fields (set when isCherryPick is true).
	isCherryPick bool
	pickHash     hash.Hash // the cherry-picked commit hash (or the reverted commit hash when isRevert)
	originalMsg  string    // original commit message for the cherry-pick/revert default annotation

	// Rebase-specific fields (set when isRebase is true).
	// intoHash (above) tracks the current rebased tip hash and is updated as commits are replayed.
	isRebase              bool
	ontoBranch            string      // the branch being rebased onto (the "theirs" side in conflict messages)
	rebaseBranchHash      hash.Hash   // branch HEAD before rebase started (used to reset branch on abort)
	rebaseRemainingHashes []hash.Hash // commits yet to replay (oldest-first), not including the current paused one
	rebaseCurrentPick     hash.Hash   // commit currently being replayed (paused on conflict)
	rebaseCommitsReplayed int         // number of commits successfully replayed so far

	// Revert-specific fields (set when isRevert is true).
	// pickHash (above) stores the hash of the commit being reverted.
	// fromHash (above) stores the parent hash of the reverted commit (the "theirs" side in artifacts).
	// originalMsg (above) stores the reverted commit's message for the default annotation.
	isRevert bool
}

func (m *mergeInProgress) hasUnresolvedConflicts() bool {
	for _, entries := range m.conflicts {
		for _, e := range entries {
			if !e.resolved {
				return true
			}
		}
	}
	for _, v := range m.viewConflicts {
		if !v.resolved {
			return true
		}
	}
	return false
}

func (m *mergeInProgress) theirHash() hash.Hash {
	if m.isRebase {
		return m.rebaseCurrentPick
	}
	if m.isCherryPick {
		return m.pickHash
	}
	if m.isRevert {
		// For revert: "theirs" is the parent of the reverted commit (the state we're
		// reverting to). The parent hash is stored in fromHash for revert operations.
		return m.fromHash
	}
	return m.fromHash
}

func (m *mergeInProgress) summaries() []backends.ConflictSummary {
	var out []backends.ConflictSummary
	for name, entries := range m.conflicts {
		count := 0
		for _, e := range entries {
			if !e.resolved {
				count++
			}
		}
		if count > 0 {
			out = append(out, backends.ConflictSummary{Collection: name, Count: count})
		}
	}
	for name, v := range m.viewConflicts {
		if !v.resolved {
			out = append(out, backends.ConflictSummary{Collection: name, Count: 1})
		}
	}
	return out
}

// viewConflictEntry is a view-definition conflict captured during a merge: the
// same view diverged on both branches. base/ours/theirs are the definitions on
// each side (nil where the view was absent or deleted).
type viewConflictEntry struct {
	name      string
	id        string    // conflictID hash of the view name + theirs commit hash
	base      *viewMeta // nil if the view was absent in the common ancestor
	ours      *viewMeta // nil if our branch deleted the view
	theirs    *viewMeta // nil if their branch deleted the view
	ourDiff   string    // "added", "modified", "deleted"
	theirDiff string
	resolved  bool
}

// viewSideDiff classifies one side of a view conflict relative to the base.
func viewSideDiff(baseIsView, sideIsView bool) string {
	switch {
	case !baseIsView && sideIsView:
		return "added"
	case baseIsView && !sideIsView:
		return "deleted"
	default:
		return "modified"
	}
}

// catalogConflictCollections decodes the owning collection names from a set of
// divergent __dumbo_catalog__ document conflicts (each catalog doc's _id is its
// collection name), so a merge refusal can name the affected collections without
// ever exposing the internal catalog. Names are sorted for a stable message.
func catalogConflictCollections(ctx context.Context, state *dbState, conflicts []*conflictEntry) ([]string, error) {
	var names []string
	for _, c := range conflicts {
		v := c.oursRawVal
		if v == nil {
			v = c.theirsRawVal
		}
		if v == nil {
			v = c.baseRawVal
		}
		if v == nil {
			continue
		}
		doc, err := readBSONDocFromValue(ctx, state.ns, v)
		if err != nil {
			return nil, err
		}
		if id, err := doc.Get("_id"); err == nil {
			if s, ok := id.(string); ok {
				names = append(names, s)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// conflictEntry represents a single document-level conflict captured during a merge.
// The raw key and value tuples are stored as byte slices so they can be decoded lazily on demand.
type conflictEntry struct {
	id     string
	typ    string    // "documentEdit" (default) or "uniqueKeyCollision"
	rawKey val.Tuple // encoded _id key bytes (the document identity for a documentEdit)

	// Per-side key overrides. For a documentEdit all three coincide with
	// rawKey. For a uniqueKeyCollision the contending documents have
	// different _ids, so ours and theirs carry their own keys.
	oursKey   val.Tuple // nil -> rawKey
	theirsKey val.Tuple // nil -> rawKey

	baseRawVal    val.Tuple // base document value tuple (nil if document was absent in ancestor)
	oursRawVal    val.Tuple // ours document value tuple (nil if our branch deleted the document)
	theirsRawVal  val.Tuple // theirs document value tuple (nil if their branch deleted the document)
	ourDiffType   string    // "added", "modified", or "deleted"
	theirDiffType string    // "added", "modified", or "deleted"

	// reason explains why this is a conflict.
	reasonCode    string
	reasonMessage string
	reasonIndex   string          // unique index name, for uniqueKeyCollision
	reasonKey     *types.Document // colliding key value, for uniqueKeyCollision

	resolved bool
}

func diffTypeString(dt tree.DiffType) string {
	switch dt {
	case tree.AddedDiff:
		return "added"
	case tree.ModifiedDiff:
		return "modified"
	case tree.RemovedDiff:
		return "deleted"
	default:
		return "none"
	}
}

func documentEditReasonCode(ourDiff, theirDiff string) string {
	switch {
	case ourDiff == "deleted" && theirDiff != "deleted":
		return "deleteModify"
	case ourDiff != "deleted" && theirDiff == "deleted":
		return "modifyDelete"
	default:
		return "bothModified"
	}
}

// documentEditReasonMessage describes a same-identity edit clash, naming the
// document _id and each side. oursDesc/theirsDesc identify the sides, e.g.
// "branch 'main' (ours)" and "branch 'feature' (theirs)".
func documentEditReasonMessage(code string, id any, oursDesc, theirsDesc string) string {
	idStr := types.FormatAnyValue(id)
	switch code {
	case "deleteModify":
		return fmt.Sprintf("%s deleted document %s; %s modified it", oursDesc, idStr, theirsDesc)
	case "modifyDelete":
		return fmt.Sprintf("%s modified document %s; %s deleted it", oursDesc, idStr, theirsDesc)
	default:
		return fmt.Sprintf("%s and %s both modified document %s", oursDesc, theirsDesc, idStr)
	}
}

// collisionMessage describes a unique-index collision, naming the index and
// each contending side (e.g. "branch 'main' (ours)").
func collisionMessage(index string, key *types.Document, oursDesc, theirsDesc string) string {
	return fmt.Sprintf("unique index %q: %s and %s both have %s", index, oursDesc, theirsDesc, renderKey(key))
}

func renderKey(key *types.Document) string {
	if key == nil {
		return "the same key"
	}
	parts := make([]string, 0, len(key.Keys()))
	for _, k := range key.Keys() {
		v, _ := key.Get(k)
		if s, ok := v.(string); ok {
			parts = append(parts, fmt.Sprintf("%s = %q", k, s))
		} else {
			parts = append(parts, fmt.Sprintf("%s = %v", k, v))
		}
	}
	return strings.Join(parts, ", ")
}

// DumboDBConflicts implements backends.VersioningBackend.
//
// When ConflictsParams.Collection is empty it returns per-collection conflict counts.
// When ConflictsParams.Collection is set it returns per-conflict details for that collection.
func (b *Backend) DumboDBConflicts(ctx context.Context, params *backends.ConflictsParams) (*backends.ConflictsResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBConflicts: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBConflicts: database %q does not exist", params.DBName))
	}

	db.mu.RLock()
	ms := db.mergeState
	db.mu.RUnlock()

	if ms == nil {
		return nil, fmt.Errorf("DumboDBConflicts: no merge or cherry-pick in progress on branch %q", params.Branch)
	}

	var collections []backends.CollectionConflicts

	for collName, entries := range ms.conflicts {
		var infos []backends.ConflictInfo
		for _, e := range entries {
			if e.resolved {
				continue
			}

			typ := e.typ
			if typ == "" {
				typ = "documentEdit"
			}
			info := backends.ConflictInfo{
				ConflictID:    e.id,
				Type:          typ,
				OurDiffType:   e.ourDiffType,
				TheirDiffType: e.theirDiffType,
				Reason: backends.ConflictReason{
					Code:    e.reasonCode,
					Message: e.reasonMessage,
					Index:   e.reasonIndex,
					Key:     e.reasonKey,
				},
			}

			oursKey := e.oursKey
			if oursKey == nil {
				oursKey = e.rawKey
			}
			theirsKey := e.theirsKey
			if theirsKey == nil {
				theirsKey = e.rawKey
			}

			if e.baseRawVal != nil {
				doc, docErr := readDocFromEntry(ctx, db.ns, e.rawKey, e.baseRawVal)
				if docErr != nil {
					return nil, fmt.Errorf("DumboDBConflicts: reading base doc for conflict %q: %w", e.id, docErr)
				}
				info.Base = doc
			}

			if e.oursRawVal != nil {
				doc, docErr := readDocFromEntry(ctx, db.ns, oursKey, e.oursRawVal)
				if docErr != nil {
					return nil, fmt.Errorf("DumboDBConflicts: reading ours doc for conflict %q: %w", e.id, docErr)
				}
				info.Ours = doc
			}

			if e.theirsRawVal != nil {
				doc, docErr := readDocFromEntry(ctx, db.ns, theirsKey, e.theirsRawVal)
				if docErr != nil {
					return nil, fmt.Errorf("DumboDBConflicts: reading theirs doc for conflict %q: %w", e.id, docErr)
				}
				info.Theirs = doc
			}

			infos = append(infos, info)
		}

		if len(infos) > 0 {
			collections = append(collections, backends.CollectionConflicts{
				Collection: collName,
				Conflicts:  infos,
			})
		}
	}

	if collections == nil {
		collections = []backends.CollectionConflicts{}
	}

	var views []backends.ViewConflict
	for _, v := range ms.viewConflicts {
		if v.resolved {
			continue
		}
		views = append(views, backends.ViewConflict{
			Name:          v.name,
			ConflictID:    v.id,
			Base:          viewMetaToDefinition(v.base),
			Ours:          viewMetaToDefinition(v.ours),
			Theirs:        viewMetaToDefinition(v.theirs),
			OurDiffType:   v.ourDiff,
			TheirDiffType: v.theirDiff,
		})
	}
	if views == nil {
		views = []backends.ViewConflict{}
	}

	return &backends.ConflictsResult{Collections: collections, Views: views}, nil
}

// viewMetaToDefinition converts a stored view definition to the backends type
// used in conflict/diff results; nil in, nil out.
func viewMetaToDefinition(vm *viewMeta) *backends.ViewDefinition {
	if vm == nil {
		return nil
	}
	return &backends.ViewDefinition{ViewOn: vm.ViewOn, Pipeline: vm.Pipeline}
}

// DumboDBResolveConflict implements backends.VersioningBackend.
//
// Resolves a single document conflict in the current in-progress merge.
// After resolution the conflict entry is marked resolved and the resolvedAM is
// updated to reflect the chosen document state.
func (b *Backend) DumboDBResolveConflict(ctx context.Context, params *backends.ResolveConflictParams) (*backends.ResolveConflictResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("DumboDBResolveConflict: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	ms := db.mergeState
	if ms == nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: no merge or cherry-pick in progress on branch %q", params.Branch)
	}

	if vce, ok := ms.viewConflicts[params.Collection]; ok {
		return b.resolveViewConflict(ctx, db, ms, vce, params)
	}

	entries, ok := ms.conflicts[params.Collection]
	if !ok {
		return nil, fmt.Errorf("DumboDBResolveConflict: collection %q has no conflicts", params.Collection)
	}

	var target *conflictEntry
	for _, e := range entries {
		if e.id == params.ConflictID {
			target = e
			break
		}
	}

	if target == nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: conflict %q not found in collection %q", params.ConflictID, params.Collection)
	}
	if target.resolved {
		return nil, fmt.Errorf("DumboDBResolveConflict: conflict %q is already resolved", params.ConflictID)
	}

	// For "ours", the resolvedAM already has our value  -- just mark resolved, no AM update needed.
	if params.Resolution == "ours" {
		target.resolved = true
		return &backends.ResolveConflictResult{}, nil
	}

	var chosenVal val.Tuple
	var deleteDoc bool

	switch params.Resolution {
	case "theirs":
		chosenVal = target.theirsRawVal
		deleteDoc = target.theirsRawVal == nil

	case "custom":
		if params.Value == nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: resolution %q requires a value document", params.Resolution)
		}
		v, writeErr := writeDocToValue(ctx, db.ns, params.Value)
		if writeErr != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: writing custom document: %w", writeErr)
		}
		chosenVal = v

	default:
		return nil, fmt.Errorf("DumboDBResolveConflict: unknown resolution %q (must be 'ours', 'theirs', or 'custom')", params.Resolution)
	}

	collMap, err := collectionMapFromAM(ctx, db, ms.resolvedAM, params.Collection)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: opening collection %q from resolvedAM: %w", params.Collection, err)
	}

	// Resolution is a write: the chosen state is re-indexed and must
	// not collide on a unique index.
	resolveIdxAM, err := indexAMFromAM(ctx, db.cs, db.ns, ms.resolvedAM, params.Collection)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: reading index AM for %q: %w", params.Collection, err)
	}
	idxSet, err := indexSetFromAM(ctx, db, resolveIdxAM)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: resolving indexes for %q: %w", params.Collection, err)
	}
	idxInfos := make([]backends.IndexInfo, 0, len(idxSet))
	idxMaps := make(map[string]prolly.Map, len(idxSet))
	for _, entry := range idxSet {
		m, merr := openIndexMap(ctx, db.vs, db.ns, entry.mapRoot)
		if merr != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: opening index %q: %w", entry.info.Name, merr)
		}
		idxInfos = append(idxInfos, entry.info)
		idxMaps[entry.info.Name] = m
	}
	sort.Slice(idxInfos, func(i, j int) bool { return idxInfos[i].Name < idxInfos[j].Name })

	// A uniqueKeyCollision resolved with "theirs" is a key-ownership
	// swap: evict ours's surviving document so theirs's contender can
	// take the key. Un-index the winner up front so the collision probe
	// below sees a free key, and remember its key for the collection
	// delete.
	collisionSwap := target.typ == "uniqueKeyCollision" && params.Resolution == "theirs" &&
		target.oursRawVal != nil && target.oursKey != nil
	var swapWinnerKey val.Tuple
	if collisionSwap {
		swapWinnerKey = target.oursKey
		winnerDoc, derr := readDocFromValue(ctx, db.ns, target.oursRawVal)
		if derr != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: decoding surviving document: %w", derr)
		}
		idxMaps, err = applyDeletesToIndexes(ctx, idxInfos, idxMaps, []*types.Document{winnerDoc})
		if err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: un-indexing evicted winner: %w", err)
		}
	}

	rawKeyIDBytes, ok := keyDesc.GetBytes(0, target.rawKey)
	if !ok {
		return nil, fmt.Errorf("DumboDBResolveConflict: conflict key missing id bytes")
	}
	var selfHash [20]byte
	copy(selfHash[:], rawKeyIDBytes)

	var oldDoc *types.Document
	if err := collMap.Get(ctx, target.rawKey, func(_, v val.Tuple) error {
		if v == nil {
			return nil
		}
		var derr error
		oldDoc, derr = readDocFromValue(ctx, db.ns, v)
		return derr
	}); err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: reading current document: %w", err)
	}

	var chosenDoc *types.Document
	if !deleteDoc {
		chosenDoc, err = readDocFromValue(ctx, db.ns, chosenVal)
		if err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: decoding chosen document: %w", err)
		}
		// C4: a colliding resolution is rejected; conflict stays open.
		for _, idx := range idxInfos {
			if !idx.Unique {
				continue
			}
			rows, _, lossy := indexEntriesForDoc(chosenDoc, idx)
			if len(rows) == 0 || lossy || idx.Lossy {
				continue
			}
			for _, row := range rows {
				conflict, perr := idxpkg.UniqueConflict(ctx, idxMaps[idx.Name], row, selfHash[:])
				if perr != nil {
					return nil, fmt.Errorf("DumboDBResolveConflict: unique probe on %s: %w", idx.Name, perr)
				}
				if conflict {
					return nil, backends.NewError(
						backends.ErrorCodeInsertDuplicateID,
						fmt.Errorf("resolution collides with a different document on unique index %s", idx.Name),
					)
				}
			}
		}
	}

	mut := collMap.Mutate()

	if collisionSwap {
		if err := mut.Delete(ctx, swapWinnerKey); err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: evicting surviving document from collection %q: %w", params.Collection, err)
		}
	}

	if deleteDoc {
		if err := mut.Delete(ctx, target.rawKey); err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: deleting document from collection %q: %w", params.Collection, err)
		}
	} else {
		if err := mut.Put(ctx, target.rawKey, chosenVal); err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: updating document in collection %q: %w", params.Collection, err)
		}
	}

	newCollMap, err := mut.Map(ctx)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: flushing collection map for %q: %w", params.Collection, err)
	}

	var resolvedIdxInfos []backends.IndexInfo
	resolvedIdxMaps := idxMaps
	if deleteDoc {
		resolvedIdxInfos = idxInfos
		if oldDoc != nil {
			resolvedIdxMaps, err = applyDeletesToIndexes(ctx, idxInfos, idxMaps, []*types.Document{oldDoc})
			if err != nil {
				return nil, fmt.Errorf("DumboDBResolveConflict: un-indexing deleted document: %w", err)
			}
		}
	} else if oldDoc != nil {
		resolvedIdxInfos, resolvedIdxMaps, err = applyUpdatesToIndexes(ctx, idxInfos, idxMaps,
			[]*types.Document{oldDoc}, []*types.Document{chosenDoc})
		if err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: re-indexing resolved document: %w", err)
		}
	} else {
		resolvedIdxInfos, resolvedIdxMaps, err = applyInsertsToIndexes(ctx, idxInfos, idxMaps,
			[]*types.Document{chosenDoc})
		if err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: indexing resolved document: %w", err)
		}
	}
	newIdxAM, err := buildIndexAM(ctx, db, resolvedIdxInfos, resolvedIdxMaps)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: building index AM: %w", err)
	}

	updatedAM, err := removeConflictArtifact(ctx, db, ms.resolvedAM, params.Collection, target.rawKey, ms.theirHash())
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: updating artifact map for %q: %w", params.Collection, err)
	}

	// Now rebuild the DTBL for the collection with the updated document map and artifact hash.
	// The removeConflictArtifact call above already updated the AM's DTBL hash for the artifacts.
	// We need to further update it with the new collection map (document changes).
	newArtHash := hash.Hash{}
	{
		// Re-read the artifacts hash from the updated AM's DTBL (after removeConflictArtifact).
		h, getErr := updatedAM.Get(ctx, params.Collection)
		if getErr == nil && !h.IsEmpty() {
			chunk, chunkErr := db.cs.Get(ctx, h)
			if chunkErr == nil {
				fileID := serial.GetFileID(chunk.Data())
				if fileID == serial.TableFileID {
					tbl, tblErr := serial.TryGetRootAsTable(chunk.Data(), serial.MessagePrefixSz)
					if tblErr == nil {
						newArtHash = hash.New(tbl.ArtifactsBytes())
					}
				}
			}
		}
	}

	newCollCount, countErr := newCollMap.Count()
	if countErr != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: counting new collection map for %q: %w", params.Collection, countErr)
	}

	amEditor := updatedAM.Editor()
	if newCollCount == 0 {
		if err := amEditor.Delete(ctx, params.Collection); err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: deleting collection %q from AM: %w", params.Collection, err)
		}
	} else {
		newCollHash, hashErr := db.dtblHashForCollection(ctx, params.Collection, newCollMap, newIdxAM, newArtHash)
		if hashErr != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: getting DTBL hash for %q: %w", params.Collection, hashErr)
		}
		if err := amEditor.Update(ctx, params.Collection, newCollHash); err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: updating AM for collection %q: %w", params.Collection, err)
		}
	}

	finalAM, err := amEditor.Flush(ctx)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: flushing AM editor: %w", err)
	}

	ms.resolvedAM = finalAM
	target.resolved = true

	// Update the in-memory working set AM so that doltDiff and doltStatus
	// reflect the resolved state immediately.
	db.setAM(ctx, ms.intoBranch, finalAM)

	// Update the working set so that dolt_conflicts SQL tables immediately reflect
	// the resolved state.
	_, stageErr := headRootAMForBranch(ctx, db, ms.intoBranch)
	if stageErr != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: reading staged AM: %w", stageErr)
	}
	workingRtvl := buildRootValueFlatbuffer(finalAM)
	if _, writeErr := db.vs.WriteValue(ctx, dolttypes.SerialMessage(workingRtvl)); writeErr != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: writing working RTVL: %w", writeErr)
	}
	if wsErr := db.persistAM(ctx, ms.intoBranch, finalAM); wsErr != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: updating working set: %w", wsErr)
	}

	return &backends.ResolveConflictResult{}, nil
}

// viewMetaFromDoc builds a view definition from a custom-resolution document
// carrying {viewOn, pipeline, collation}.
func viewMetaFromDoc(doc *types.Document) *viewMeta {
	vm := &viewMeta{}
	if v, err := doc.Get("viewOn"); err == nil {
		vm.ViewOn, _ = v.(string)
	}
	if p, err := doc.Get("pipeline"); err == nil {
		vm.Pipeline, _ = p.(*types.Array)
	}
	if c, err := doc.Get("collation"); err == nil {
		vm.Collation, _ = c.(*types.Document)
	}
	return vm
}

// resolveViewConflict resolves a single view-definition merge conflict by
// choosing ours, theirs, or a custom definition, updating the resolved
// AddressMap accordingly. The caller holds db.mu (write lock).
func (b *Backend) resolveViewConflict(ctx context.Context, db *dbState, ms *mergeInProgress, vce *viewConflictEntry, params *backends.ResolveConflictParams) (*backends.ResolveConflictResult, error) {
	if vce.resolved {
		return nil, fmt.Errorf("DumboDBResolveConflict: view conflict %q is already resolved", params.ConflictID)
	}

	// "ours" is already reflected in resolvedAM (kept during capture); just mark
	// resolved. The other resolutions rewrite the view entry.
	if params.Resolution == "ours" {
		vce.resolved = true
		return &backends.ResolveConflictResult{}, nil
	}

	var newDef *viewMeta // nil => the resolution deletes the view
	switch params.Resolution {
	case "theirs":
		newDef = vce.theirs
	case "custom":
		if params.Value == nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: resolution %q requires a value document", params.Resolution)
		}
		newDef = viewMetaFromDoc(params.Value)
	default:
		return nil, fmt.Errorf("DumboDBResolveConflict: unknown resolution %q (must be 'ours', 'theirs', or 'custom')", params.Resolution)
	}

	editor := ms.resolvedAM.Editor()
	has, err := ms.resolvedAM.Has(ctx, params.Collection)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: probing resolved AM for view %q: %w", params.Collection, err)
	}
	if newDef == nil {
		// Resolution deletes the view.
		if has {
			if derr := editor.Delete(ctx, params.Collection); derr != nil {
				return nil, fmt.Errorf("DumboDBResolveConflict: deleting view %q: %w", params.Collection, derr)
			}
		}
	} else {
		viewHash, werr := writeViewChunk(ctx, db.ns, newDef)
		if werr != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: writing resolved view %q: %w", params.Collection, werr)
		}
		if has {
			err = editor.Update(ctx, params.Collection, viewHash)
		} else {
			err = editor.Add(ctx, params.Collection, viewHash)
		}
		if err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: updating view %q in AM: %w", params.Collection, err)
		}
	}

	finalAM, err := editor.Flush(ctx)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: flushing AM editor: %w", err)
	}

	ms.resolvedAM = finalAM
	vce.resolved = true

	db.setAM(ctx, ms.intoBranch, finalAM)
	workingRtvl := buildRootValueFlatbuffer(finalAM)
	if _, writeErr := db.vs.WriteValue(ctx, dolttypes.SerialMessage(workingRtvl)); writeErr != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: writing working RTVL: %w", writeErr)
	}
	if wsErr := db.persistAM(ctx, ms.intoBranch, finalAM); wsErr != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: updating working set: %w", wsErr)
	}

	return &backends.ResolveConflictResult{}, nil
}

// buildConflictArtifactHash creates an ArtifactMap for the given conflict entries,
// writes the node to the value store, and returns its hash.
// theirHash is the "theirs" commit hash; baseHash is the common ancestor commit hash.
func buildConflictArtifactHash(ctx context.Context, state *dbState, entries []*conflictEntry, theirHash, baseHash hash.Hash) (hash.Hash, error) {
	am, err := prolly.NewArtifactMapFromTuples(ctx, state.ns, keyDesc)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("creating artifact map: %w", err)
	}
	edt := am.Editor()

	metaBytes, err := json.Marshal(prolly.ConflictMetadata{BaseRootIsh: baseHash})
	if err != nil {
		return hash.Hash{}, fmt.Errorf("encoding conflict metadata: %w", err)
	}

	for _, e := range entries {
		if err := edt.Add(ctx, e.rawKey, theirHash, prolly.ArtifactTypeConflict, metaBytes, nil); err != nil {
			return hash.Hash{}, fmt.Errorf("adding conflict artifact for key %q: %w", e.id, err)
		}
	}

	newAM, err := edt.Flush(ctx)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("flushing artifact map: %w", err)
	}

	ref, err := state.vs.WriteValue(ctx, tree.ValueFromNode(newAM.Node()))
	if err != nil {
		return hash.Hash{}, fmt.Errorf("writing artifact map: %w", err)
	}
	return ref.TargetHash(), nil
}

// openCollectionArtifacts reads the ArtifactMap for a collection from the DTBL
// artifacts field in the given AddressMap. Returns an empty ArtifactMap if the
// collection is not present or has no artifacts hash.
func openCollectionArtifacts(ctx context.Context, state *dbState, am prolly.AddressMap, collName string) (prolly.ArtifactMap, error) {
	h, err := am.Get(ctx, collName)
	if err != nil || h.IsEmpty() {
		return prolly.NewArtifactMapFromTuples(ctx, state.ns, keyDesc)
	}

	chunk, err := state.cs.Get(ctx, h)
	if err != nil {
		return prolly.ArtifactMap{}, fmt.Errorf("reading DTBL chunk for %q: %w", collName, err)
	}

	fileID := serial.GetFileID(chunk.Data())
	if fileID != serial.TableFileID {
		// Not a DTBL; no artifact support.
		return prolly.NewArtifactMapFromTuples(ctx, state.ns, keyDesc)
	}

	tbl, err := serial.TryGetRootAsTable(chunk.Data(), serial.MessagePrefixSz)
	if err != nil {
		return prolly.ArtifactMap{}, fmt.Errorf("parsing DTBL for %q: %w", collName, err)
	}

	artHash := hash.New(tbl.ArtifactsBytes())
	if artHash.IsEmpty() {
		return prolly.NewArtifactMapFromTuples(ctx, state.ns, keyDesc)
	}

	v, err := state.vs.ReadValue(ctx, artHash)
	if err != nil {
		return prolly.ArtifactMap{}, fmt.Errorf("reading artifact map for %q: %w", collName, err)
	}

	node, _, err := tree.NodeFromBytes(v.(dolttypes.SerialMessage))
	if err != nil {
		return prolly.ArtifactMap{}, fmt.Errorf("parsing artifact map node for %q: %w", collName, err)
	}

	return prolly.NewArtifactMap(node, state.ns, keyDesc), nil
}

// removeConflictArtifact removes the conflict artifact for the given rawKey from
// the collection's ArtifactMap in the AM. The DTBL is updated in-place via the
// AM editor; if the ArtifactMap becomes empty the DTBL reverts to zero artifacts.
// Returns the updated AM.
func removeConflictArtifact(ctx context.Context, state *dbState, am prolly.AddressMap, collName string, rawKey val.Tuple, theirHash hash.Hash) (prolly.AddressMap, error) {
	artMap, err := openCollectionArtifacts(ctx, state, am, collName)
	if err != nil {
		return am, fmt.Errorf("opening artifacts for %q: %w", collName, err)
	}

	edt := artMap.Editor()
	artKey, err := edt.BuildArtifactKey(ctx, rawKey, theirHash, prolly.ArtifactTypeConflict, nil)
	if err != nil {
		return am, fmt.Errorf("building artifact key for conflict in %q: %w", collName, err)
	}

	if err := edt.Delete(ctx, artKey); err != nil {
		return am, fmt.Errorf("deleting artifact for conflict in %q: %w", collName, err)
	}

	newArtMap, err := edt.Flush(ctx)
	if err != nil {
		return am, fmt.Errorf("flushing artifact map for %q: %w", collName, err)
	}

	newArtHash := hash.Hash{}
	if cnt, countErr := newArtMap.Count(); countErr == nil && cnt > 0 {
		ref, writeErr := state.vs.WriteValue(ctx, tree.ValueFromNode(newArtMap.Node()))
		if writeErr != nil {
			return am, fmt.Errorf("writing updated artifact map for %q: %w", collName, writeErr)
		}
		newArtHash = ref.TargetHash()
	}

	collMap, err := collectionMapFromAM(ctx, state, am, collName)
	if err != nil {
		return am, fmt.Errorf("opening collection map for %q: %w", collName, err)
	}

	curIdxAM, err := indexAMFromAM(ctx, state.cs, state.ns, am, collName)
	if err != nil {
		return am, fmt.Errorf("reading current index AM for %q: %w", collName, err)
	}

	newDTBLHash, err := state.dtblHashForCollection(ctx, collName, collMap, curIdxAM, newArtHash)
	if err != nil {
		return am, fmt.Errorf("building DTBL for %q: %w", collName, err)
	}

	amEdt := am.Editor()
	if err := amEdt.Update(ctx, collName, newDTBLHash); err != nil {
		return am, fmt.Errorf("updating AM for %q: %w", collName, err)
	}
	return amEdt.Flush(ctx)
}

// conflictID computes a conflict ID matching dolt's dolt_conflict_id column.
// It hashes the key tuple concatenated with the theirRootIsh commit hash using
// XXH3-128, then base64-encodes the 16-byte result (raw standard encoding, no
// padding). This matches dolt's GetConflictId in conflicts_tables_prolly.go.
func conflictID(rawKey val.Tuple, theirHash hash.Hash) string {
	b := xxh3.Hash128(append([]byte(rawKey), theirHash[:]...)).Bytes()
	return base64.RawStdEncoding.EncodeToString(b[:])
}

// captureConflictsForCollection merges two collection maps at document level,
// capturing conflicts instead of erroring. Returns the partial merged map (keeping
// "ours" for conflicting documents) and the list of captured conflict entries.
//
// When applier is non-nil, every document-level change also routes
// through it so the surviving indexes stay consistent with the merged
// primary (B2-B6); unique collisions evict the loser ("ours wins") and
// become ordinary document conflicts.
func captureConflictsForCollection(
	ctx context.Context,
	intoMap, fromMap, baseMap prolly.Map,
	theirHash hash.Hash,
	applier *indexMergeApplier,
	oursDesc, theirsDesc string,
) (mergedMap prolly.Map, entries []*conflictEntry, err error) {
	ns := baseMap.NodeStore()

	tryMergeJSON := func(_ *sql.Context, left, right, base val.Tuple) (val.Tuple, bool, error) {
		if left == nil || right == nil || len(left) == 0 || len(right) == 0 {
			return nil, false, nil
		}
		leftDoc, err := readDocFromValue(ctx, ns, left)
		if err != nil {
			return nil, false, nil
		}
		rightDoc, err := readDocFromValue(ctx, ns, right)
		if err != nil {
			return nil, false, nil
		}
		var baseDoc *types.Document
		if base != nil && len(base) > 0 {
			baseDoc, err = readDocFromValue(ctx, ns, base)
			if err != nil {
				baseDoc = nil
			}
		}
		if baseDoc == nil {
			baseDoc = types.MakeDocument(0)
		}
		mergedDoc, conflict := mergeBSONDoc(baseDoc, leftDoc, rightDoc)
		if conflict {
			return nil, false, nil
		}
		mergedVal, err := writeDocToValue(ctx, ns, mergedDoc)
		if err != nil {
			return nil, false, nil
		}
		return mergedVal, true, nil
	}

	differ, err := tree.NewThreeWayDiffer[val.Tuple, val.Tuple, *val.TupleDesc](
		ctx, ns,
		intoMap.Tuples(), fromMap.Tuples(), baseMap.Tuples(),
		tryMergeJSON,
		false, // not keyless
		tree.ThreeWayDiffInfo{},
		intoMap.KeyDesc(),
	)
	if err != nil {
		return prolly.Map{}, nil, fmt.Errorf("creating three-way differ: %w", err)
	}
	defer differ.Close()

	// Start from the from (theirs) map and apply left-only changes on top.
	// This avoids key-encoding mismatches from tree re-chunking between
	// the into and from maps. Conflicts keep "ours" (left) values.
	mut := fromMap.Mutate()

	indexing := applier != nil && len(applier.survivors) > 0

	docOf := func(tup val.Tuple) (*types.Document, error) {
		if len(tup) == 0 {
			return nil, nil
		}
		return readDocFromValue(ctx, ns, tup)
	}

	// applyIndexEdit runs the unique-collision eviction protocol: the
	// loser leaves the merged primary and becomes a document conflict.
	applyIndexEdit := func(edit mergeDocEdit, key val.Tuple) error {
		if !indexing {
			return nil
		}
		rawKey := append(val.Tuple(nil), key...)
		idBytes, ok := keyDesc.GetBytes(0, rawKey)
		if !ok {
			return fmt.Errorf("merge index maintenance: primary key tuple missing id bytes")
		}
		for attempt := 0; ; attempt++ {
			if attempt > 2 {
				return fmt.Errorf("merge index maintenance: unique-collision eviction did not converge")
			}
			loser, err := applier.apply(ctx, edit, idBytes)
			if err != nil {
				return err
			}
			if loser == nil {
				return nil
			}
			if loser.incoming {
				// Revert the merged primary to the ours/base state and
				// record a conflict carrying theirs.
				var baseVal, theirsVal val.Tuple
				if edit.base != nil {
					bv, werr := writeDocToValue(ctx, ns, edit.base)
					if werr != nil {
						return werr
					}
					baseVal = bv
				}
				if edit.right != nil {
					tv, werr := writeDocToValue(ctx, ns, edit.right)
					if werr != nil {
						return werr
					}
					theirsVal = tv
				}
				if baseVal != nil {
					if err := mut.Put(ctx, key, baseVal); err != nil {
						return err
					}
				} else if err := mut.Delete(ctx, key); err != nil {
					return err
				}
				revert := mergeDocEdit{kind: editKeepOurs, right: edit.right, left: edit.base}
				if _, err := applier.apply(ctx, revert, idBytes); err != nil {
					return err
				}
				e := &conflictEntry{
					id:            conflictID(rawKey, theirHash),
					typ:           "uniqueKeyCollision",
					rawKey:        rawKey,
					baseRawVal:    baseVal,
					oursRawVal:    baseVal,
					theirsRawVal:  theirsVal,
					reasonCode:    "uniqueKeyCollision",
					reasonIndex:   loser.index,
					reasonKey:     loser.key,
					reasonMessage: collisionMessage(loser.index, loser.key, oursDesc, theirsDesc),
				}
				e.ourDiffType = "modified"
				if baseVal == nil {
					e.ourDiffType = "deleted"
				}
				e.theirDiffType = "modified"
				if edit.base == nil {
					e.theirDiffType = "added"
				}
				entries = append(entries, e)
				return nil
			}
			// The existing claim loses: evict it everywhere, record the
			// conflict, retry this edit.
			loserKey, kerr := buildKey(loser.id)
			if kerr != nil {
				return kerr
			}
			var loserVal val.Tuple
			var loserDoc *types.Document
			if gerr := mut.Get(ctx, loserKey, func(_, v val.Tuple) error {
				if v == nil {
					return nil
				}
				loserVal = append(val.Tuple(nil), v...)
				var derr error
				loserDoc, derr = readDocFromValue(ctx, ns, v)
				return derr
			}); gerr != nil {
				return gerr
			}
			if loserDoc == nil {
				return fmt.Errorf("merge index maintenance: unique collision against a missing document")
			}
			if err := applier.removeDocEverywhere(ctx, loserDoc, loser.id); err != nil {
				return err
			}
			if err := mut.Delete(ctx, loserKey); err != nil {
				return err
			}
			// ours = the surviving incoming document (it keeps the key on
			// retry); theirs = the evicted existing document. They are
			// distinct identities sharing one indexed key.
			var oursVal val.Tuple
			ourDiff := "added"
			if edit.left != nil {
				ov, werr := writeDocToValue(ctx, ns, edit.left)
				if werr != nil {
					return werr
				}
				oursVal = ov
				if edit.base != nil {
					ourDiff = "modified"
				}
			}
			// rawKey is the parked (loser) identity that resolution acts
			// on; oursKey points at the surviving winner for display only.
			entries = append(entries, &conflictEntry{
				id:            conflictID(loserKey, theirHash),
				typ:           "uniqueKeyCollision",
				rawKey:        loserKey,
				oursKey:       rawKey,
				oursRawVal:    oursVal,
				theirsRawVal:  loserVal,
				ourDiffType:   ourDiff,
				theirDiffType: "added",
				reasonCode:    "uniqueKeyCollision",
				reasonIndex:   loser.index,
				reasonKey:     loser.key,
				reasonMessage: collisionMessage(loser.index, loser.key, oursDesc, theirsDesc),
			})
		}
	}

	sqlCtx := sql.NewEmptyContext()
	for {
		diff, err := differ.Next(sqlCtx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return prolly.Map{}, nil, fmt.Errorf("three-way differ: %w", err)
		}

		switch diff.Op {
		// Left-only changes: apply to the merged map (starting from the from map).
		case tree.DiffOpLeftAdd, tree.DiffOpLeftModify:
			if err := mut.Put(ctx, diff.Key, diff.Left); err != nil {
				return prolly.Map{}, nil, fmt.Errorf("applying left change: %w", err)
			}
			if indexing {
				baseDoc, derr := docOf(val.Tuple(diff.Base))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				leftDoc, derr := docOf(val.Tuple(diff.Left))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				if err := applyIndexEdit(mergeDocEdit{kind: editLeftChange, base: baseDoc, left: leftDoc}, diff.Key); err != nil {
					return prolly.Map{}, nil, err
				}
			}
		case tree.DiffOpLeftDelete:
			if err := mut.Delete(ctx, diff.Key); err != nil {
				return prolly.Map{}, nil, fmt.Errorf("applying left delete: %w", err)
			}
			if indexing {
				baseDoc, derr := docOf(val.Tuple(diff.Base))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				if err := applyIndexEdit(mergeDocEdit{kind: editLeftChange, base: baseDoc}, diff.Key); err != nil {
					return prolly.Map{}, nil, err
				}
			}

		// Already in the from map; INTO-seeded indexes still need them.
		case tree.DiffOpRightAdd, tree.DiffOpRightModify, tree.DiffOpRightDelete:
			if indexing {
				baseDoc, derr := docOf(val.Tuple(diff.Base))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				rightDoc, derr := docOf(val.Tuple(diff.Right))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				if err := applyIndexEdit(mergeDocEdit{kind: editRightChange, base: baseDoc, right: rightDoc}, diff.Key); err != nil {
					return prolly.Map{}, nil, err
				}
			}

		// Both sides made the same change: present everywhere already.
		case tree.DiffOpConvergentAdd, tree.DiffOpConvergentModify, tree.DiffOpConvergentDelete:

		// Divergent edits: real conflicts. Keep "ours" (left) value in the merged map.
		case tree.DiffOpDivergentModifyConflict, tree.DiffOpDivergentDeleteConflict:
			// Clone everything retained beyond this iteration: the
			// differ reuses its tuple buffers between Next calls.
			rawKey := append(val.Tuple(nil), diff.Key...)
			entry := &conflictEntry{
				id:     conflictID(rawKey, theirHash),
				typ:    "documentEdit",
				rawKey: rawKey,
			}
			if diff.Base != nil {
				entry.baseRawVal = append(val.Tuple(nil), diff.Base...)
			}
			if diff.Left != nil {
				entry.oursRawVal = append(val.Tuple(nil), diff.Left...)
				entry.ourDiffType = "modified"
				// Override from's value with ours in the merged map.
				if err := mut.Put(ctx, diff.Key, diff.Left); err != nil {
					return prolly.Map{}, nil, fmt.Errorf("applying ours for conflict: %w", err)
				}
			} else {
				entry.ourDiffType = "deleted"
				// Our side deleted; remove from the merged map.
				if err := mut.Delete(ctx, diff.Key); err != nil {
					return prolly.Map{}, nil, fmt.Errorf("deleting for our-side delete conflict: %w", err)
				}
			}
			if diff.Right != nil {
				entry.theirsRawVal = append(val.Tuple(nil), diff.Right...)
				entry.theirDiffType = "modified"
			} else {
				entry.theirDiffType = "deleted"
			}
			var idVal any = types.Null
			for _, rv := range []val.Tuple{entry.oursRawVal, entry.theirsRawVal, entry.baseRawVal} {
				if rv == nil {
					continue
				}
				d, derr := readDocFromValue(ctx, ns, rv)
				if derr != nil {
					return prolly.Map{}, nil, fmt.Errorf("decoding conflict document for _id: %w", derr)
				}
				if v, gerr := d.Get("_id"); gerr == nil {
					idVal = v
					break
				}
			}
			entry.reasonCode = documentEditReasonCode(entry.ourDiffType, entry.theirDiffType)
			entry.reasonMessage = documentEditReasonMessage(entry.reasonCode, idVal, oursDesc, theirsDesc)
			entries = append(entries, entry)
			if indexing {
				leftDoc, derr := docOf(val.Tuple(diff.Left))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				rightDoc, derr := docOf(val.Tuple(diff.Right))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				if err := applyIndexEdit(mergeDocEdit{kind: editKeepOurs, left: leftDoc, right: rightDoc}, diff.Key); err != nil {
					return prolly.Map{}, nil, err
				}
			}

		// Resolved divergent edits: the JSON field merge succeeded. Apply
		// the merged value to the map.
		case tree.DiffOpDivergentModifyResolved:
			if err := mut.Put(ctx, diff.Key, diff.Merged); err != nil {
				return prolly.Map{}, nil, fmt.Errorf("applying resolved merge: %w", err)
			}
			if indexing {
				leftDoc, derr := docOf(val.Tuple(diff.Left))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				rightDoc, derr := docOf(val.Tuple(diff.Right))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				mergedDoc, derr := docOf(val.Tuple(diff.Merged))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				if err := applyIndexEdit(mergeDocEdit{kind: editResolved, left: leftDoc, right: rightDoc, merged: mergedDoc}, diff.Key); err != nil {
					return prolly.Map{}, nil, err
				}
			}
		case tree.DiffOpDivergentDeleteResolved:
			// Delete-modify resolved by callback (unlikely for JSON merge).
			if diff.Merged == nil {
				if err := mut.Delete(ctx, diff.Key); err != nil {
					return prolly.Map{}, nil, fmt.Errorf("applying resolved delete: %w", err)
				}
			} else {
				if err := mut.Put(ctx, diff.Key, diff.Merged); err != nil {
					return prolly.Map{}, nil, fmt.Errorf("applying resolved merge: %w", err)
				}
			}
			if indexing {
				leftDoc, derr := docOf(val.Tuple(diff.Left))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				rightDoc, derr := docOf(val.Tuple(diff.Right))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				mergedDoc, derr := docOf(val.Tuple(diff.Merged))
				if derr != nil {
					return prolly.Map{}, nil, derr
				}
				if err := applyIndexEdit(mergeDocEdit{kind: editResolved, left: leftDoc, right: rightDoc, merged: mergedDoc}, diff.Key); err != nil {
					return prolly.Map{}, nil, err
				}
			}
		}
	}

	mergedMap, err = mut.Map(ctx)
	if err != nil {
		return prolly.Map{}, nil, fmt.Errorf("flushing merged map: %w", err)
	}

	return mergedMap, entries, nil
}

// mergeAddressMapsWithConflicts performs a 3-way merge of collections AddressMaps,
// capturing document-level conflicts rather than erroring on them.
//
// Collection-level conflicts (entire collection deleted on one branch while modified on the other)
// are still returned as hard errors.
//
// theirHash is the "theirs" commit hash (merge source / cherry-pick / rebase pick).
// baseHash is the common ancestor commit hash. Both are stored in the ArtifactMap for
// each conflicting collection so that dolt_conflicts SQL tables can read the artifacts.
//
// Returns the partial merged AM (with "ours" values for conflicting documents) and a
// per-collection map of captured conflict entries. The conflicts map is non-nil but may be empty.
func mergeAddressMapsWithConflicts(ctx context.Context, state *dbState, intoAM, fromAM, baseAM prolly.AddressMap, theirHash, baseHash hash.Hash, oursDesc, theirsDesc string) (prolly.AddressMap, map[string][]*conflictEntry, map[string]*viewConflictEntry, error) {
	viewConflicts := map[string]*viewConflictEntry{}
	// Includes both collections and views: the AddressMap-level lifecycle logic
	// below (add/update/delete of an entry hash) is identical for either. Only
	// the both-sides-modified branch differs, where a view is diffed as a
	// definition rather than a document map (see the isViewEntry check there).
	allNames := make(map[string]struct{})
	for _, am := range []prolly.AddressMap{intoAM, fromAM, baseAM} {
		if err := am.IterAll(ctx, func(name string, _ hash.Hash) error {
			allNames[name] = struct{}{}
			return nil
		}); err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("iterating collections AM: %w", err)
		}
	}

	editor := intoAM.Editor()
	allConflicts := make(map[string][]*conflictEntry)

	for name := range allNames {
		intoH, err := intoAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("reading into AM for %q: %w", name, err)
		}
		fromH, err := fromAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("reading from AM for %q: %w", name, err)
		}
		baseH, err := baseAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("reading base AM for %q: %w", name, err)
		}

		intoChanged := intoH != baseH
		fromChanged := fromH != baseH

		if !fromChanged {
			// From did not touch this collection; keep Into's version.
			continue
		}

		if !intoChanged {
			// Only From changed this collection; apply From's change.
			switch {
			case fromH.IsEmpty():
				if err := editor.Delete(ctx, name); err != nil {
					return prolly.AddressMap{}, nil, nil, fmt.Errorf("deleting collection %q: %w", name, err)
				}
			case intoH.IsEmpty():
				if err := editor.Add(ctx, name, fromH); err != nil {
					return prolly.AddressMap{}, nil, nil, fmt.Errorf("adding collection %q: %w", name, err)
				}
			default:
				if err := editor.Update(ctx, name, fromH); err != nil {
					return prolly.AddressMap{}, nil, nil, fmt.Errorf("updating collection %q: %w", name, err)
				}
			}
			continue
		}

		if fromH.IsEmpty() && intoH.IsEmpty() {
			continue
		}

		// A view (or a collection-vs-view name clash) cannot be document-merged.
		// When a view is involved on any side and both sides diverged, capture a
		// view-definition conflict: keep ours in the resolved AM (the editor
		// already holds it) and record base/ours/theirs for interactive
		// resolution via doltConflicts/doltResolveConflict.
		intoIsView, err := isViewEntry(ctx, state.cs, intoH)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, err
		}
		fromIsView, err := isViewEntry(ctx, state.cs, fromH)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, err
		}
		baseIsView, err := isViewEntry(ctx, state.cs, baseH)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, err
		}
		if intoIsView || fromIsView || baseIsView {
			vce := &viewConflictEntry{
				name:      name,
				id:        conflictID(val.Tuple(name), theirHash),
				ourDiff:   viewSideDiff(baseIsView, intoIsView),
				theirDiff: viewSideDiff(baseIsView, fromIsView),
			}
			if intoIsView {
				if vm, rerr := readViewChunk(ctx, state.ns, intoH); rerr == nil {
					vce.ours = vm
				}
			}
			if fromIsView {
				if vm, rerr := readViewChunk(ctx, state.ns, fromH); rerr == nil {
					vce.theirs = vm
				}
			}
			if baseIsView {
				if vm, rerr := readViewChunk(ctx, state.ns, baseH); rerr == nil {
					vce.base = vm
				}
			}
			viewConflicts[name] = vce
			continue
		}

		if fromH.IsEmpty() || intoH.IsEmpty() {
			// One side deleted, the other modified -- unresolvable here.
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("conflict in %q: deleted on one branch and modified on the other", name)
		}

		// Both sides modified the collection  -- merge at document level, capturing conflicts.
		intoMap, err := openCollection(ctx, state.cs, state.ns, intoH)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("opening into collection %q: %w", name, err)
		}
		fromMap, err := openCollection(ctx, state.cs, state.ns, fromH)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("opening from collection %q: %w", name, err)
		}
		var baseMap prolly.Map
		if baseH.IsEmpty() {
			baseMap, err = newEmptyMap(ctx, state.ns)
		} else {
			baseMap, err = openCollection(ctx, state.cs, state.ns, baseH)
		}
		if err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("opening base collection %q: %w", name, err)
		}

		// Reconcile index definitions (B5) and seed survivors; the
		// differ below drives the other side's edits through them.
		intoIdxAM, idxErr := indexAMForDTBL(ctx, state.cs, state.ns, intoH)
		if idxErr != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("reading into index AM for %q: %w", name, idxErr)
		}
		fromIdxAM, idxErr := indexAMForDTBL(ctx, state.cs, state.ns, fromH)
		if idxErr != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("reading from index AM for %q: %w", name, idxErr)
		}
		var baseIdxAM prolly.AddressMap
		if baseH.IsEmpty() {
			baseIdxAM, idxErr = prolly.NewEmptyAddressMap(state.ns)
		} else {
			baseIdxAM, idxErr = indexAMForDTBL(ctx, state.cs, state.ns, baseH)
		}
		if idxErr != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("reading base index AM for %q: %w", name, idxErr)
		}
		intoSet, idxErr := indexSetFromAM(ctx, state, intoIdxAM)
		if idxErr != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("resolving into indexes for %q: %w", name, idxErr)
		}
		fromSet, idxErr := indexSetFromAM(ctx, state, fromIdxAM)
		if idxErr != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("resolving from indexes for %q: %w", name, idxErr)
		}
		baseSet, idxErr := indexSetFromAM(ctx, state, baseIdxAM)
		if idxErr != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("resolving base indexes for %q: %w", name, idxErr)
		}
		_, seeds, idxErr := reconcileIndexSets(intoSet, fromSet, baseSet)
		if idxErr != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("merging indexes of %q: %w", name, idxErr)
		}
		survivors, idxErr := openSurvivors(ctx, state, seeds)
		if idxErr != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("opening merged indexes of %q: %w", name, idxErr)
		}
		applier := &indexMergeApplier{state: state, survivors: survivors}

		mergedMap, collConflicts, err := captureConflictsForCollection(ctx, intoMap, fromMap, baseMap, theirHash, applier, oursDesc, theirsDesc)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("merging collection %q: %w", name, err)
		}

		// The internal metadata catalog must never surface as a raw conflict
		// (users must never see reservedCatalogName). Non-conflicting per-
		// collection metadata docs merge cleanly. But a DIVERGENT metadata doc
		// -- the same collection's validator/options changed on both branches --
		// must NOT be silently dropped: that would lose one side's change
		// invisibly. Until the metadata conflict-resolution workflow lands
		// (workspace-xhm surfaces it as a resolvable conflict on the owning
		// collection), refuse the merge loudly, naming the owning collection(s)
		// rather than the internal catalog.
		if name == reservedCatalogName {
			if len(collConflicts) > 0 {
				colls, nerr := catalogConflictCollections(ctx, state, collConflicts)
				if nerr != nil {
					return prolly.AddressMap{}, nil, nil, nerr
				}
				return prolly.AddressMap{}, nil, nil, fmt.Errorf(
					"cannot merge: collection metadata (validator/options) changed on both branches for %s; "+
						"automatic metadata conflict resolution is not yet supported -- align the metadata on "+
						"both branches before merging", strings.Join(colls, ", "))
			}
			collConflicts = nil
		}

		var artHash hash.Hash // zero = no artifacts
		if len(collConflicts) > 0 {
			allConflicts[name] = collConflicts

			// Build and write ArtifactMap so dolt_conflicts SQL tables can read conflicts.
			artHash, err = buildConflictArtifactHash(ctx, state, collConflicts, theirHash, baseHash)
			if err != nil {
				return prolly.AddressMap{}, nil, nil, fmt.Errorf("building conflict artifacts for %q: %w", name, err)
			}
		}

		mergedIdxAM, idxErr := applier.finalize(ctx)
		if idxErr != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("finalizing merged indexes of %q: %w", name, idxErr)
		}
		mergedH, err := state.dtblHashForCollection(ctx, name, mergedMap, mergedIdxAM, artHash)
		if err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("writing merged collection %q: %w", name, err)
		}
		if err := editor.Update(ctx, name, mergedH); err != nil {
			return prolly.AddressMap{}, nil, nil, fmt.Errorf("updating merged collection %q in AM: %w", name, err)
		}
	}

	am, err := editor.Flush(ctx)
	if err != nil {
		return prolly.AddressMap{}, nil, nil, err
	}

	return am, allConflicts, viewConflicts, nil
}
