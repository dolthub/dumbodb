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

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/libraries/doltcore/merge"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/dolthub/go-mysql-server/sql"
	sqltypes "github.com/dolthub/go-mysql-server/sql/types"
	"github.com/zeebo/xxh3"

	"github.com/dolthub/dumbodb/internal/backends"
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
	fromBranch  string
	intoBranch  string
	premergeAM  prolly.AddressMap // ours branch AM before the operation started (used to abort)
	fromHash    hash.Hash         // fromBranch HEAD hash at merge time (merge commit parent 2)
	intoHash    hash.Hash         // intoBranch HEAD hash at merge/cherry-pick time (commit parent 1)
	// conflicts: collection name -> ordered list of conflict entries.
	// Entries are never removed; resolved ones have resolved==true.
	conflicts map[string][]*conflictEntry
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

// hasUnresolvedConflicts reports whether any conflict entry in the merge state is unresolved.
func (m *mergeInProgress) hasUnresolvedConflicts() bool {
	for _, entries := range m.conflicts {
		for _, e := range entries {
			if !e.resolved {
				return true
			}
		}
	}
	return false
}

// theirHash returns the "their" commit hash for artifact storage, depending on the
// operation type.
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

// summaries builds a per-collection list of unresolved conflict counts.
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
	return out
}

// conflictEntry represents a single document-level conflict captured during a merge.
// The raw key and value tuples are stored as byte slices so they can be decoded lazily on demand.
type conflictEntry struct {
	id            string
	rawKey        val.Tuple // encoded _id key bytes
	baseRawVal    val.Tuple // base document value tuple (nil if document was absent in ancestor)
	oursRawVal    val.Tuple // ours document value tuple (nil if our branch deleted the document)
	theirsRawVal  val.Tuple // theirs document value tuple (nil if their branch deleted the document)
	ourDiffType   string    // "added", "modified", or "deleted"
	theirDiffType string    // "added", "modified", or "deleted"
	resolved      bool
}

// diffTypeString converts a tree.DiffType to the canonical string used in the wire protocol.
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

			info := backends.ConflictInfo{
				ConflictID:    e.id,
				OurDiffType:   e.ourDiffType,
				TheirDiffType: e.theirDiffType,
			}

			if e.baseRawVal != nil {
				doc, docErr := readDocFromEntry(ctx, db.ns, e.rawKey, e.baseRawVal)
				if docErr != nil {
					return nil, fmt.Errorf("DumboDBConflicts: reading base doc for conflict %q: %w", e.id, docErr)
				}
				info.Base = doc
			}

			if e.oursRawVal != nil {
				doc, docErr := readDocFromEntry(ctx, db.ns, e.rawKey, e.oursRawVal)
				if docErr != nil {
					return nil, fmt.Errorf("DumboDBConflicts: reading ours doc for conflict %q: %w", e.id, docErr)
				}
				info.Ours = doc
			}

			if e.theirsRawVal != nil {
				doc, docErr := readDocFromEntry(ctx, db.ns, e.rawKey, e.theirsRawVal)
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

	return &backends.ConflictsResult{Collections: collections}, nil
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

	// Determine the chosen value for "theirs" or "custom".
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
		jsonHash, writeErr := writeDocJSON(ctx, db.ns, params.Value)
		if writeErr != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: writing custom document: %w", writeErr)
		}
		v, buildErr := buildValue(jsonHash)
		if buildErr != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: building value tuple: %w", buildErr)
		}
		chosenVal = v

	default:
		return nil, fmt.Errorf("DumboDBResolveConflict: unknown resolution %q (must be 'ours', 'theirs', or 'custom')", params.Resolution)
	}

	// Update the collection map in resolvedAM to reflect the chosen resolution.
	collMap, err := collectionMapFromAM(ctx, db, ms.resolvedAM, params.Collection)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: opening collection %q from resolvedAM: %w", params.Collection, err)
	}

	mut := collMap.Mutate()

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

	// Remove the resolved conflict from the ArtifactMap for this collection.
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
		newCollHash, hashErr := db.dtblHashForCollection(ctx, params.Collection, newCollMap, newArtHash)
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
	db.setAM(ms.intoBranch, finalAM)

	// Update the working set so that dolt_conflicts SQL tables immediately reflect
	// the resolved state.
	stagedAM, stageErr := headRootAMForBranch(ctx, db, ms.intoBranch)
	if stageErr != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: reading staged AM: %w", stageErr)
	}
	workingRtvl := buildRootValueFlatbuffer(finalAM)
	if _, writeErr := db.vs.WriteValue(ctx, dolttypes.SerialMessage(workingRtvl)); writeErr != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: writing working RTVL: %w", writeErr)
	}
	if wsErr := updateWorkingSet(ctx, db.doltDB, finalAM, stagedAM, ms.intoBranch); wsErr != nil {
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

	// Determine the new artifacts hash (zero if empty).
	newArtHash := hash.Hash{}
	if cnt, countErr := newArtMap.Count(); countErr == nil && cnt > 0 {
		ref, writeErr := state.vs.WriteValue(ctx, tree.ValueFromNode(newArtMap.Node()))
		if writeErr != nil {
			return am, fmt.Errorf("writing updated artifact map for %q: %w", collName, writeErr)
		}
		newArtHash = ref.TargetHash()
	}

	// Get the current collection map to rebuild the DTBL.
	collMap, err := collectionMapFromAM(ctx, state, am, collName)
	if err != nil {
		return am, fmt.Errorf("opening collection map for %q: %w", collName, err)
	}

	newDTBLHash, err := state.dtblHashForCollection(ctx, collName, collMap, newArtHash)
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
func captureConflictsForCollection(
	ctx context.Context,
	intoMap, fromMap, baseMap prolly.Map,
	theirHash hash.Hash,
) (mergedMap prolly.Map, entries []*conflictEntry, err error) {
	ns := baseMap.NodeStore()

	// Resolve callback: attempt field-level JSON merge using Dolt's
	// three-way JSON differ. Non-overlapping field changes are merged
	// automatically; overlapping changes produce a conflict.
	tryMergeJSON := func(_ *sql.Context, left, right, base val.Tuple) (val.Tuple, bool, error) {
		// If either side is a delete (nil tuple), we can't merge JSON fields.
		if left == nil || right == nil || len(left) == 0 || len(right) == 0 {
			return nil, false, nil
		}

		// Extract JSON addresses from value tuples.
		leftAddr, lok := valDesc.GetJSONAddr(0, left)
		rightAddr, rok := valDesc.GetJSONAddr(0, right)
		if !lok || !rok {
			return nil, false, nil
		}

		var baseDoc sql.JSONWrapper
		if base != nil {
			if baseAddr, ok := valDesc.GetJSONAddr(0, base); ok {
				doc, docErr := tree.NewJSONDoc(baseAddr, ns).ToIndexedJSONDocument(ctx)
				if docErr != nil {
					return nil, false, nil
				}
				baseDoc = doc
			}
		}
		if baseDoc == nil {
			// No base (both sides added) -- create an empty base.
			emptyJSON := sqltypes.NewLazyJSONDocument([]byte("{}"))
			root, serErr := tree.SerializeJsonToAddr(ctx, ns, emptyJSON)
			if serErr != nil {
				return nil, false, nil
			}
			baseDoc = tree.NewIndexedJsonDocument(ctx, root, ns)
		}

		leftDoc, err := tree.NewJSONDoc(leftAddr, ns).ToIndexedJSONDocument(ctx)
		if err != nil {
			return nil, false, nil
		}
		rightDoc, err := tree.NewJSONDoc(rightAddr, ns).ToIndexedJSONDocument(ctx)
		if err != nil {
			return nil, false, nil
		}

		mergedDoc, conflict, err := merge.MergeJSON(ctx, ns, baseDoc, leftDoc, rightDoc)
		if err != nil || conflict {
			return nil, false, nil
		}

		// Write the merged JSON and build a value tuple.
		mergedRoot, err := tree.SerializeJsonToAddr(ctx, ns, mergedDoc)
		if err != nil {
			return nil, false, nil
		}
		mergedVal, err := buildValue(mergedRoot.HashOf())
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
		case tree.DiffOpLeftDelete:
			if err := mut.Delete(ctx, diff.Key); err != nil {
				return prolly.Map{}, nil, fmt.Errorf("applying left delete: %w", err)
			}

		// Right-only changes: already in the from map, nothing to do.
		case tree.DiffOpRightAdd, tree.DiffOpRightModify, tree.DiffOpRightDelete:
			// no-op

		// Convergent edits: both sides made the same change. Already in from map
		// for add/modify; for delete, already absent.
		case tree.DiffOpConvergentAdd, tree.DiffOpConvergentModify, tree.DiffOpConvergentDelete:
			// no-op

		// Divergent edits: real conflicts. Keep "ours" (left) value in the merged map.
		case tree.DiffOpDivergentModifyConflict, tree.DiffOpDivergentDeleteConflict:
			rawKey := val.Tuple(diff.Key)
			entry := &conflictEntry{
				id:     conflictID(rawKey, theirHash),
				rawKey: rawKey,
			}
			if diff.Base != nil {
				entry.baseRawVal = val.Tuple(diff.Base)
			}
			if diff.Left != nil {
				entry.oursRawVal = val.Tuple(diff.Left)
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
				entry.theirsRawVal = val.Tuple(diff.Right)
				entry.theirDiffType = "modified"
			} else {
				entry.theirDiffType = "deleted"
			}
			entries = append(entries, entry)

		// Resolved divergent edits: the JSON field merge succeeded. Apply
		// the merged value to the map.
		case tree.DiffOpDivergentModifyResolved:
			if err := mut.Put(ctx, diff.Key, diff.Merged); err != nil {
				return prolly.Map{}, nil, fmt.Errorf("applying resolved merge: %w", err)
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
func mergeAddressMapsWithConflicts(ctx context.Context, state *dbState, intoAM, fromAM, baseAM prolly.AddressMap, theirHash, baseHash hash.Hash) (prolly.AddressMap, map[string][]*conflictEntry, error) {
	allNames := make(map[string]struct{})
	for _, am := range []prolly.AddressMap{intoAM, fromAM, baseAM} {
		if err := am.IterAll(ctx, func(name string, _ hash.Hash) error {
			allNames[name] = struct{}{}
			return nil
		}); err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("iterating collections AM: %w", err)
		}
	}

	editor := intoAM.Editor()
	allConflicts := make(map[string][]*conflictEntry)

	for name := range allNames {
		intoH, err := intoAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("reading into AM for %q: %w", name, err)
		}
		fromH, err := fromAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("reading from AM for %q: %w", name, err)
		}
		baseH, err := baseAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("reading base AM for %q: %w", name, err)
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
					return prolly.AddressMap{}, nil, fmt.Errorf("deleting collection %q: %w", name, err)
				}
			case intoH.IsEmpty():
				if err := editor.Add(ctx, name, fromH); err != nil {
					return prolly.AddressMap{}, nil, fmt.Errorf("adding collection %q: %w", name, err)
				}
			default:
				if err := editor.Update(ctx, name, fromH); err != nil {
					return prolly.AddressMap{}, nil, fmt.Errorf("updating collection %q: %w", name, err)
				}
			}
			continue
		}

		// Both sides changed this collection.
		if fromH.IsEmpty() && intoH.IsEmpty() {
			// Both independently deleted the collection; result is deletion (already absent).
			continue
		}
		if fromH.IsEmpty() || intoH.IsEmpty() {
			// Collection-level conflict: one side deleted, the other modified  -- unresolvable here.
			return prolly.AddressMap{}, nil, fmt.Errorf("conflict in collection %q: deleted on one branch and modified on the other", name)
		}

		// Both sides modified the collection  -- merge at document level, capturing conflicts.
		intoMap, err := openCollection(ctx, state.cs, state.ns, intoH)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("opening into collection %q: %w", name, err)
		}
		fromMap, err := openCollection(ctx, state.cs, state.ns, fromH)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("opening from collection %q: %w", name, err)
		}
		baseMap, err := openCollection(ctx, state.cs, state.ns, baseH)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("opening base collection %q: %w", name, err)
		}

		mergedMap, collConflicts, err := captureConflictsForCollection(ctx, intoMap, fromMap, baseMap, theirHash)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("merging collection %q: %w", name, err)
		}

		var artHash hash.Hash // zero = no artifacts
		if len(collConflicts) > 0 {
			allConflicts[name] = collConflicts

			// Build and write ArtifactMap so dolt_conflicts SQL tables can read conflicts.
			artHash, err = buildConflictArtifactHash(ctx, state, collConflicts, theirHash, baseHash)
			if err != nil {
				return prolly.AddressMap{}, nil, fmt.Errorf("building conflict artifacts for %q: %w", name, err)
			}
		}

		mergedH, err := state.dtblHashForCollection(ctx, name, mergedMap, artHash)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("writing merged collection %q: %w", name, err)
		}
		if err := editor.Update(ctx, name, mergedH); err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("updating merged collection %q in AM: %w", name, err)
		}
	}

	am, err := editor.Flush(ctx)
	if err != nil {
		return prolly.AddressMap{}, nil, err
	}

	return am, allConflicts, nil
}
