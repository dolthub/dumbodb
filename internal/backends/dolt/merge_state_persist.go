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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
)

const mergeStateFileName = ".dumbodb_merge_state.json"

// mergeStateDisk is the JSON-serializable form of mergeInProgress.
// Only operation metadata is stored; conflict entries are reconstructed
// from the ArtifactMap stored in the DTBL for each conflicting collection.
type mergeStateDisk struct {
	Operation  string `json:"op"` // "merge", "cherry-pick", "rebase"
	IntoBranch string `json:"into"`
	FromBranch string `json:"from,omitempty"` // merge only
	OntoBranch string `json:"onto,omitempty"` // rebase only
	FromHash   string `json:"fromHash,omitempty"`
	IntoHash   string `json:"intoHash"`
	PremergeAM string `json:"premergeAM"` // hash of the premerge AddressMap RTVL

	// Cherry-pick specific.
	PickHash    string `json:"pickHash,omitempty"`
	OriginalMsg string `json:"originalMsg,omitempty"`

	// Rebase specific.
	RebaseBranchHash      string   `json:"rebaseBranchHash,omitempty"`
	RebaseRemainingHashes []string `json:"rebaseRemaining,omitempty"`
	RebaseCurrentPick     string   `json:"rebaseCurrentPick,omitempty"`
	RebaseCommitsReplayed int      `json:"rebaseReplayed,omitempty"`

	// Collections that have conflict artifacts in the working set.
	ConflictCollections []string `json:"conflictCols"`

	ViewConflicts []viewConflictDisk `json:"viewConflicts,omitempty"`

	MetaConflicts []metaConflictDisk `json:"metaConflicts,omitempty"`
}

type metaConflictDisk struct {
	Coll          string `json:"coll"`
	ID            string `json:"id"`
	OurDiff       string `json:"ourDiff"`
	TheirDiff     string `json:"theirDiff"`
	ReasonCode    string `json:"reasonCode,omitempty"`
	ReasonMessage string `json:"reasonMessage,omitempty"`
	Resolved      bool   `json:"resolved"`
	BaseHex       string `json:"base,omitempty"`
	OursHex       string `json:"ours,omitempty"`
	TheirsHex     string `json:"theirs,omitempty"`
}

type viewConflictDisk struct {
	Name      string `json:"name"`
	ID        string `json:"id"`
	OurDiff   string `json:"ourDiff"`
	TheirDiff string `json:"theirDiff"`
	Resolved  bool   `json:"resolved"`
	BaseHex   string `json:"base,omitempty"`
	OursHex   string `json:"ours,omitempty"`
	TheirsHex string `json:"theirs,omitempty"`
}

func (state *dbState) mergeStateFilePath() string {
	return filepath.Join(state.dbDir, mergeStateFileName)
}

// saveMergeState writes the merge state to disk. It also updates the working set
// to reflect the partial merged AM (so dolt_conflicts SQL tables are readable).
// The caller must hold state.mu (write lock).
func saveMergeState(ctx context.Context, state *dbState, ms *mergeInProgress) error {
	var op string
	switch {
	case ms.isRebase:
		op = "rebase"
	case ms.isCherryPick:
		op = "cherry-pick"
	case ms.isRevert:
		op = "revert"
	default:
		op = "merge"
	}

	// Collect names of collections with unresolved conflicts.
	var conflictCols []string
	for name := range ms.conflicts {
		conflictCols = append(conflictCols, name)
	}

	// Write the premerge AM to the value store so we can reload it on restart.
	preMergeRtvl := buildRootValueFlatbuffer(ms.premergeAM)
	preMergeRef, err := state.vs.WriteValue(ctx, dolttypes.SerialMessage(preMergeRtvl))
	if err != nil {
		return fmt.Errorf("writing premerge AM RTVL: %w", err)
	}

	disk := mergeStateDisk{
		Operation:             op,
		IntoBranch:            ms.intoBranch,
		FromBranch:            ms.fromBranch,
		OntoBranch:            ms.ontoBranch,
		FromHash:              ms.fromHash.String(),
		IntoHash:              ms.intoHash.String(),
		PremergeAM:            preMergeRef.TargetHash().String(),
		PickHash:              ms.pickHash.String(),
		OriginalMsg:           ms.originalMsg,
		RebaseCommitsReplayed: ms.rebaseCommitsReplayed,
		ConflictCollections:   conflictCols,
	}

	for _, v := range ms.viewConflicts {
		vd := viewConflictDisk{Name: v.name, ID: v.id, OurDiff: v.ourDiff, TheirDiff: v.theirDiff, Resolved: v.resolved}
		if v.base != nil {
			if vd.BaseHex, err = viewMetaToBSONHex(v.base); err != nil {
				return fmt.Errorf("encoding base view conflict %q: %w", v.name, err)
			}
		}
		if v.ours != nil {
			if vd.OursHex, err = viewMetaToBSONHex(v.ours); err != nil {
				return fmt.Errorf("encoding ours view conflict %q: %w", v.name, err)
			}
		}
		if v.theirs != nil {
			if vd.TheirsHex, err = viewMetaToBSONHex(v.theirs); err != nil {
				return fmt.Errorf("encoding theirs view conflict %q: %w", v.name, err)
			}
		}
		disk.ViewConflicts = append(disk.ViewConflicts, vd)
	}

	for _, mc := range ms.metaConflicts {
		md := metaConflictDisk{Coll: mc.coll, ID: mc.id, OurDiff: mc.ourDiff, TheirDiff: mc.theirDiff, ReasonCode: mc.reasonCode, ReasonMessage: mc.reasonMessage, Resolved: mc.resolved}
		if mc.base != nil {
			if md.BaseHex, err = collMetaToBSONHex(mc.coll, mc.base); err != nil {
				return fmt.Errorf("encoding base metadata conflict %q: %w", mc.coll, err)
			}
		}
		if mc.ours != nil {
			if md.OursHex, err = collMetaToBSONHex(mc.coll, mc.ours); err != nil {
				return fmt.Errorf("encoding ours metadata conflict %q: %w", mc.coll, err)
			}
		}
		if mc.theirs != nil {
			if md.TheirsHex, err = collMetaToBSONHex(mc.coll, mc.theirs); err != nil {
				return fmt.Errorf("encoding theirs metadata conflict %q: %w", mc.coll, err)
			}
		}
		disk.MetaConflicts = append(disk.MetaConflicts, md)
	}

	if ms.isRebase {
		disk.RebaseBranchHash = ms.rebaseBranchHash.String()
		disk.RebaseCurrentPick = ms.rebaseCurrentPick.String()
		for _, h := range ms.rebaseRemainingHashes {
			disk.RebaseRemainingHashes = append(disk.RebaseRemainingHashes, h.String())
		}
	}

	data, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling merge state: %w", err)
	}

	if err := os.WriteFile(state.mergeStateFilePath(), data, 0o644); err != nil {
		return fmt.Errorf("writing merge state file: %w", err)
	}
	return nil
}

// clearMergeState removes the merge state file from disk.
// The caller must hold state.mu (write lock).
func clearMergeState(state *dbState) error {
	path := state.mergeStateFilePath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing merge state file: %w", err)
	}
	return nil
}

// loadMergeState loads a previously persisted merge state from disk, reconstructing
// the mergeInProgress struct by reading conflict entries from the ArtifactMaps
// stored in the working set's DTBLs. Returns (nil, nil) if no state file exists.
// The caller must NOT hold state.mu (it is called from getOrOpenDB before the lock).
func loadMergeState(ctx context.Context, state *dbState) (*mergeInProgress, error) {
	path := state.mergeStateFilePath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading merge state file: %w", err)
	}

	var disk mergeStateDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		return nil, fmt.Errorf("parsing merge state file: %w", err)
	}

	// Read the current working set AM (the resolved AM from before the crash).
	resolvedAM, err := readAMFromWorkingSet(ctx, state.datasDB, state.cs, state.ns)
	if err != nil {
		return nil, fmt.Errorf("reading working set AM: %w", err)
	}

	// Reconstruct the premerge AM from its stored RTVL hash.
	preMergeAM, err := loadAMFromRTVLHash(ctx, state, disk.PremergeAM)
	if err != nil {
		return nil, fmt.Errorf("loading premerge AM: %w", err)
	}

	ms := &mergeInProgress{
		intoBranch:            disk.IntoBranch,
		fromBranch:            disk.FromBranch,
		ontoBranch:            disk.OntoBranch,
		premergeAM:            preMergeAM,
		resolvedAM:            resolvedAM,
		isCherryPick:          disk.Operation == "cherry-pick",
		isRebase:              disk.Operation == "rebase",
		isRevert:              disk.Operation == "revert",
		originalMsg:           disk.OriginalMsg,
		rebaseCommitsReplayed: disk.RebaseCommitsReplayed,
	}

	if h, ok := hash.MaybeParse(disk.FromHash); ok {
		ms.fromHash = h
	}
	if h, ok := hash.MaybeParse(disk.IntoHash); ok {
		ms.intoHash = h
	}
	if h, ok := hash.MaybeParse(disk.PickHash); ok {
		ms.pickHash = h
	}
	if ms.isRebase {
		if h, ok := hash.MaybeParse(disk.RebaseBranchHash); ok {
			ms.rebaseBranchHash = h
		}
		if h, ok := hash.MaybeParse(disk.RebaseCurrentPick); ok {
			ms.rebaseCurrentPick = h
		}
		for _, hs := range disk.RebaseRemainingHashes {
			if h, ok := hash.MaybeParse(hs); ok {
				ms.rebaseRemainingHashes = append(ms.rebaseRemainingHashes, h)
			}
		}
	}

	// Reconstruct conflict entries from ArtifactMaps in the working set.
	ms.conflicts = make(map[string][]*conflictEntry)
	for _, collName := range disk.ConflictCollections {
		entries, err := loadConflictEntriesFromArtifacts(ctx, state, resolvedAM, collName, ms)
		if err != nil {
			return nil, fmt.Errorf("loading conflicts for %q from artifacts: %w", collName, err)
		}
		if len(entries) > 0 {
			ms.conflicts[collName] = entries
		}
	}

	if len(disk.ViewConflicts) > 0 {
		ms.viewConflicts = make(map[string]*viewConflictEntry, len(disk.ViewConflicts))
		for _, vd := range disk.ViewConflicts {
			vce := &viewConflictEntry{name: vd.Name, id: vd.ID, ourDiff: vd.OurDiff, theirDiff: vd.TheirDiff, resolved: vd.Resolved}
			if vd.BaseHex != "" {
				if vce.base, err = viewMetaFromBSONHex(vd.BaseHex); err != nil {
					return nil, fmt.Errorf("decoding base view conflict %q: %w", vd.Name, err)
				}
			}
			if vd.OursHex != "" {
				if vce.ours, err = viewMetaFromBSONHex(vd.OursHex); err != nil {
					return nil, fmt.Errorf("decoding ours view conflict %q: %w", vd.Name, err)
				}
			}
			if vd.TheirsHex != "" {
				if vce.theirs, err = viewMetaFromBSONHex(vd.TheirsHex); err != nil {
					return nil, fmt.Errorf("decoding theirs view conflict %q: %w", vd.Name, err)
				}
			}
			ms.viewConflicts[vd.Name] = vce
		}
	}

	if len(disk.MetaConflicts) > 0 {
		ms.metaConflicts = make(map[string]*metaConflictEntry, len(disk.MetaConflicts))
		for _, md := range disk.MetaConflicts {
			mce := &metaConflictEntry{coll: md.Coll, id: md.ID, ourDiff: md.OurDiff, theirDiff: md.TheirDiff, reasonCode: md.ReasonCode, reasonMessage: md.ReasonMessage, resolved: md.Resolved}
			if md.BaseHex != "" {
				if mce.base, err = collMetaFromBSONHex(md.BaseHex); err != nil {
					return nil, fmt.Errorf("decoding base metadata conflict %q: %w", md.Coll, err)
				}
			}
			if md.OursHex != "" {
				if mce.ours, err = collMetaFromBSONHex(md.OursHex); err != nil {
					return nil, fmt.Errorf("decoding ours metadata conflict %q: %w", md.Coll, err)
				}
			}
			if md.TheirsHex != "" {
				if mce.theirs, err = collMetaFromBSONHex(md.TheirsHex); err != nil {
					return nil, fmt.Errorf("decoding theirs metadata conflict %q: %w", md.Coll, err)
				}
			}
			ms.metaConflicts[md.Coll] = mce
		}
	}

	return ms, nil
}

// loadAMFromRTVLHash reads a collections AddressMap from the RTVL chunk at addr.
func loadAMFromRTVLHash(ctx context.Context, state *dbState, addrStr string) (prolly.AddressMap, error) {
	if addrStr == "" {
		return prolly.NewEmptyAddressMap(state.ns)
	}
	h, ok := hash.MaybeParse(addrStr)
	if !ok || h.IsEmpty() {
		return prolly.NewEmptyAddressMap(state.ns)
	}

	v, err := state.vs.ReadValue(ctx, h)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("reading RTVL at %q: %w", addrStr, err)
	}

	msg, msgOK := v.(dolttypes.SerialMessage)
	if !msgOK {
		return prolly.AddressMap{}, fmt.Errorf("unexpected value type at %q: %T", addrStr, v)
	}

	rtvl, parseErr := serial.TryGetRootAsRootValue([]byte(msg), serial.MessagePrefixSz)
	if parseErr != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing RTVL at %q: %w", addrStr, parseErr)
	}

	amNode, _, nodeErr := tree.NodeFromBytes(rtvl.TablesBytes())
	if nodeErr != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing AM from RTVL at %q: %w", addrStr, nodeErr)
	}

	return prolly.NewAddressMap(amNode, state.ns)
}

// loadConflictEntriesFromArtifacts reads the ArtifactMap for collName from the
// working set AM and reconstructs conflict entries. For each conflict artifact,
// it looks up base/ours/theirs values from the stored commit hashes.
func loadConflictEntriesFromArtifacts(ctx context.Context, state *dbState, resolvedAM prolly.AddressMap, collName string, ms *mergeInProgress) ([]*conflictEntry, error) {
	artMap, err := openCollectionArtifacts(ctx, state, resolvedAM, collName)
	if err != nil {
		return nil, fmt.Errorf("opening artifacts for %q: %w", collName, err)
	}

	cnt, err := artMap.Count()
	if err != nil || cnt == 0 {
		return nil, nil
	}

	// Load the collection map from the resolved AM for "ours" values.
	oursMap, err := collectionMapFromAM(ctx, state, resolvedAM, collName)
	if err != nil {
		return nil, fmt.Errorf("opening ours collection map for %q: %w", collName, err)
	}

	iter, err := artMap.IterAllConflicts(ctx)
	if err != nil {
		return nil, fmt.Errorf("iterating conflict artifacts for %q: %w", collName, err)
	}

	var entries []*conflictEntry
	for {
		ca, iterErr := iter.Next(ctx)
		if iterErr == io.EOF {
			break
		}
		if iterErr != nil {
			return nil, fmt.Errorf("iterating conflicts for %q: %w", collName, iterErr)
		}

		theirH := ca.TheirRootIsh
		baseH := ca.Metadata.BaseRootIsh
		rawKey := val.Tuple(ca.Key)

		// Ours: look up in the current working AM for this collection.
		var oursVal val.Tuple
		_ = oursMap.Get(ctx, rawKey, func(k, v val.Tuple) error {
			oursVal = v
			return nil
		})

		// Theirs: navigate to theirH commit -> collection map -> look up key.
		var theirsVal val.Tuple
		if !theirH.IsEmpty() {
			theirAM, amErr := amFromCommitHash(ctx, state, theirH.String())
			if amErr == nil {
				theirCollMap, cmErr := collectionMapFromAM(ctx, state, theirAM, collName)
				if cmErr == nil {
					_ = theirCollMap.Get(ctx, rawKey, func(k, v val.Tuple) error {
						theirsVal = v
						return nil
					})
				}
			}
		}

		// Base: navigate to baseH commit -> collection map -> look up key.
		var baseVal val.Tuple
		if !baseH.IsEmpty() {
			baseAM, amErr := amFromCommitHash(ctx, state, baseH.String())
			if amErr == nil {
				baseCollMap, cmErr := collectionMapFromAM(ctx, state, baseAM, collName)
				if cmErr == nil {
					_ = baseCollMap.Get(ctx, rawKey, func(k, v val.Tuple) error {
						baseVal = v
						return nil
					})
				}
			}
		}

		ourDiffType := computeDiffType(baseVal, oursVal)
		theirDiffType := computeDiffType(baseVal, theirsVal)

		entries = append(entries, &conflictEntry{
			id:            conflictID(rawKey, theirH),
			rawKey:        rawKey,
			baseRawVal:    baseVal,
			oursRawVal:    oursVal,
			theirsRawVal:  theirsVal,
			ourDiffType:   ourDiffType,
			theirDiffType: theirDiffType,
		})
	}

	return entries, nil
}

// computeDiffType returns the diff type string based on base and current values.
func computeDiffType(base, current val.Tuple) string {
	switch {
	case base == nil && current != nil:
		return "added"
	case base != nil && current == nil:
		return "deleted"
	default:
		return "modified"
	}
}

// persistConflictState writes the working set to reflect the partial merged AM
// (with ArtifactMaps in conflicting collection DTBLs) and saves the merge state
// JSON file. The caller must hold state.mu (write lock).
func persistConflictState(ctx context.Context, state *dbState, ms *mergeInProgress) error {
	// Persist the conflict state as the branch working set with working == staged
	// so `dolt checkout <branch>` sees no uncommitted diff. The conflict artifacts
	// are embedded in the resolvedAM DTBL chunks visible to `dolt sql dolt_conflicts_*`.
	if wsErr := state.persistAM(ctx, ms.intoBranch, ms.resolvedAM); wsErr != nil {
		return fmt.Errorf("updating working set: %w", wsErr)
	}

	// Save merge state to disk.
	return saveMergeState(ctx, state, ms)
}

// clearConflictArtifacts rebuilds all conflicting collection DTBLs without artifacts
// and updates ms.resolvedAM. Called before committing a resolved merge/cherry-pick/rebase.
// The caller must hold state.mu (write lock).
func clearConflictArtifacts(ctx context.Context, state *dbState, ms *mergeInProgress) error {
	amEditor := ms.resolvedAM.Editor()

	for collName := range ms.conflicts {
		// Get the collection map from the resolved AM.
		collMap, err := collectionMapFromAM(ctx, state, ms.resolvedAM, collName)
		if err != nil {
			return fmt.Errorf("opening collection %q: %w", collName, err)
		}

		// Build a DTBL with no artifacts. Preserve the current per-branch
		// index AM read from the resolved AM (workspace-ife will handle
		// proper per-index merge).
		curIdxAM, idxErr := indexAMFromAM(ctx, state.cs, state.ns, ms.resolvedAM, collName)
		if idxErr != nil {
			return fmt.Errorf("reading current index AM for %q: %w", collName, idxErr)
		}
		newDTBLHash, err := state.dtblHashForCollection(ctx, collName, collMap, curIdxAM, hash.Hash{})
		if err != nil {
			return fmt.Errorf("building clean DTBL for %q: %w", collName, err)
		}

		cnt, countErr := collMap.Count()
		if countErr != nil {
			return fmt.Errorf("counting collection %q: %w", collName, countErr)
		}

		if cnt == 0 {
			if err := amEditor.Delete(ctx, collName); err != nil {
				return fmt.Errorf("deleting empty collection %q from AM: %w", collName, err)
			}
		} else {
			if err := amEditor.Update(ctx, collName, newDTBLHash); err != nil {
				return fmt.Errorf("updating AM for %q: %w", collName, err)
			}
		}
	}

	newAM, err := amEditor.Flush(ctx)
	if err != nil {
		return fmt.Errorf("flushing AM after clearing artifacts: %w", err)
	}

	ms.resolvedAM = newAM
	return nil
}
