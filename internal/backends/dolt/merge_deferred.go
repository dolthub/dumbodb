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
	"fmt"
	"sort"

	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
)

func effectiveMetadataMergeMode(meta *collMeta) MergeMode {
	if meta == nil {
		return DefaultMergeMode
	}
	return mergeModeOrDefault(meta.MergeMode)
}

func mergedCollectionMode(ctx context.Context, state *dbState, intoAM, fromAM, baseAM prolly.AddressMap, name string) (MergeMode, error) {
	ours, err := readCatalogDoc(ctx, state, intoAM, name)
	if err != nil {
		return "", fmt.Errorf("reading ours metadata for %q: %w", name, err)
	}
	theirs, err := readCatalogDoc(ctx, state, fromAM, name)
	if err != nil {
		return "", fmt.Errorf("reading theirs metadata for %q: %w", name, err)
	}
	base, err := readCatalogDoc(ctx, state, baseAM, name)
	if err != nil {
		return "", fmt.Errorf("reading base metadata for %q: %w", name, err)
	}
	oursMode := effectiveMetadataMergeMode(ours)
	theirsMode := effectiveMetadataMergeMode(theirs)
	baseMode := effectiveMetadataMergeMode(base)
	switch {
	case oursMode == baseMode:
		return theirsMode, nil
	case theirsMode == baseMode || oursMode == theirsMode:
		return oursMode, nil
	default:
		return "", fmt.Errorf("merge mode for %q is unresolved", name)
	}
}

func setDeferredCollectionPlaceholders(ctx context.Context, mergedAM, placeholderAM prolly.AddressMap, deferred map[string]struct{}) (prolly.AddressMap, error) {
	editor := mergedAM.Editor()
	for name := range deferred {
		h, err := placeholderAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		has, err := mergedAM.Has(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		switch {
		case h.IsEmpty() && has:
			if err := editor.Delete(ctx, name); err != nil {
				return prolly.AddressMap{}, err
			}
		case !h.IsEmpty() && has:
			if err := editor.Update(ctx, name, h); err != nil {
				return prolly.AddressMap{}, err
			}
		case !h.IsEmpty():
			if err := editor.Add(ctx, name, h); err != nil {
				return prolly.AddressMap{}, err
			}
		}
	}
	return editor.Flush(ctx)
}

func operationBaseHash(ctx context.Context, state *dbState, ms *mergeInProgress) (hash.Hash, error) {
	switch {
	case ms.isSessionCommit:
		return ms.sessionBaseAM.HashOf(), nil
	case ms.isRebase:
		return firstParentHash(ctx, state, ms.rebaseCurrentPick)
	case ms.isCherryPick:
		return firstParentHash(ctx, state, ms.pickHash)
	case ms.isRevert:
		return ms.pickHash, nil
	default:
		oursCommit, err := datas.LoadCommitAddr(ctx, state.vs, ms.intoHash)
		if err != nil {
			return hash.Hash{}, fmt.Errorf("loading ours commit: %w", err)
		}
		theirsCommit, err := datas.LoadCommitAddr(ctx, state.vs, ms.fromHash)
		if err != nil {
			return hash.Hash{}, fmt.Errorf("loading theirs commit: %w", err)
		}
		baseHash, hasBase, err := datas.FindCommonAncestor(ctx, oursCommit, theirsCommit,
			state.vs, state.vs, state.ns, state.ns)
		if err != nil {
			return hash.Hash{}, fmt.Errorf("finding common ancestor: %w", err)
		}
		if !hasBase {
			return hash.Hash{}, fmt.Errorf("no common ancestor for the paused merge on %q", ms.intoBranch)
		}
		return baseHash, nil
	}
}

func mergeDeferredCollections(ctx context.Context, state *dbState, ms *mergeInProgress) (bool, error) {
	if len(ms.deferredCollections) == 0 {
		return false, nil
	}
	intoAM, fromAM, baseAM, err := operationSides(ctx, state, ms)
	if err != nil {
		return false, err
	}
	baseHash, err := operationBaseHash(ctx, state, ms)
	if err != nil {
		return false, err
	}
	oursDesc, theirsDesc := ms.sideDescriptions()
	names := make([]string, 0, len(ms.deferredCollections))
	for name := range ms.deferredCollections {
		names = append(names, name)
	}
	sort.Strings(names)

	editor := ms.resolvedAM.Editor()
	for _, name := range names {
		resolvedMeta, metaErr := readCatalogDoc(ctx, state, ms.resolvedAM, name)
		if metaErr != nil {
			return false, fmt.Errorf("reading resolved metadata for %q: %w", name, metaErr)
		}
		if resolvedMeta == nil {
			delete(ms.deferredCollections, name)
			continue
		}
		intoH, getErr := intoAM.Get(ctx, name)
		if getErr != nil {
			return false, fmt.Errorf("reading ours collection %q: %w", name, getErr)
		}
		fromH, getErr := fromAM.Get(ctx, name)
		if getErr != nil {
			return false, fmt.Errorf("reading theirs collection %q: %w", name, getErr)
		}
		baseH, getErr := baseAM.Get(ctx, name)
		if getErr != nil {
			return false, fmt.Errorf("reading base collection %q: %w", name, getErr)
		}
		if intoH.IsEmpty() || fromH.IsEmpty() {
			delete(ms.deferredCollections, name)
			continue
		}
		mergedH, conflicts, mergeErr := mergeCollectionDTBL(ctx, state, name, intoH, fromH, baseH,
			ms.theirHash(), baseHash, oursDesc, theirsDesc, mergeModeOrDefault(resolvedMeta.MergeMode))
		if mergeErr != nil {
			return false, mergeErr
		}
		if err := editor.Update(ctx, name, mergedH); err != nil {
			return false, fmt.Errorf("updating deferred collection %q: %w", name, err)
		}
		if len(conflicts) > 0 {
			ms.conflicts[name] = conflicts
		}
		delete(ms.deferredCollections, name)
	}

	mergedAM, err := editor.Flush(ctx)
	if err != nil {
		return false, fmt.Errorf("flushing deferred collections: %w", err)
	}
	ms.resolvedAM = mergedAM
	if err := crossValidateMergedDocuments(ctx, state, mergedAM, baseAM, ms.conflicts,
		map[string]*metaConflictEntry{}, ms.theirHash(), theirsDesc, false); err != nil {
		return false, fmt.Errorf("validating deferred collections: %w", err)
	}
	return ms.hasUnresolvedConflicts(), nil
}
