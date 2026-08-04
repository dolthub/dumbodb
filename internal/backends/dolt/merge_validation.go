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

	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// crossValidateMergedDocuments enforces the merge cross-validation invariant
// (workspace-h0w): a merge may never make data quality more non-conformant than
// it was. For every user collection with a settled validator whose action is
// "error", it diffs the base against the merged document map and emits a
// typ:"validation" conflict for every document the merge cleanly inserted or
// modified into a violating state. A pre-existing violator is grandfathered only
// when the merge leaves it byte-for-byte unchanged (never visited by the diff);
// a re-authored violating value is a conflict even when the base already
// violated. Under action "warn" the collection is skipped entirely (violations
// allowed, matching the write path).
//
// Documents already in a divergent data conflict are skipped here; their
// resolved value is validated at resolution time (trigger 2). A collection whose
// validator definition ITSELF diverged (an unresolved metaConflict) has no
// settled validator yet, so its cross-validation is deferred to
// recheckCrossValidation, run at merge-continue once the metaConflict is
// resolved and the validator is pinned; the merge already halts on that
// metaConflict, so nothing is silently accepted in the meantime.
//
// recheck=false is the initial pass (every collection; unresolved-metaConflict
// collections skipped; data-conflict docs skipped). recheck=true is the
// continue-time re-run: it processes ONLY collections whose metaConflict is now
// resolved, and validates every changed document (no data-conflict skip, since a
// data conflict may have been resolved against a not-yet-final validator).
// Entries whose conflictID already exists are never re-added.
func crossValidateMergedDocuments(ctx context.Context, state *dbState, mergedAM, baseAM prolly.AddressMap, allConflicts map[string][]*conflictEntry, metaConflicts map[string]*metaConflictEntry, theirHash hash.Hash, theirsDesc string, recheck bool) error {
	catMap, err := catalogMapFromAM(ctx, state, mergedAM)
	if err != nil {
		return fmt.Errorf("reading merged catalog: %w", err)
	}

	var names []string
	if err := mergedAM.IterAll(ctx, func(name string, _ hash.Hash) error {
		if name != reservedCatalogName {
			names = append(names, name)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("iterating merged collections AM: %w", err)
	}

	for _, name := range names {
		mc, hasMeta := metaConflicts[name]
		if recheck {
			if !hasMeta || !mc.resolved {
				continue // recheck touches only collections whose validator was just pinned
			}
		} else if hasMeta && !mc.resolved {
			continue // initial pass defers unsettled validators to the recheck
		}

		meta, err := readCollMetaFromCatalog(ctx, state, catMap, name)
		if err != nil {
			return fmt.Errorf("reading merged validator for %q: %w", name, err)
		}
		if meta == nil || meta.Validator == nil || meta.ValidationLevel == "off" {
			continue
		}
		if action := meta.ValidationAction; action == "warn" {
			continue
		}

		mergedMap, err := collectionMapFromAM(ctx, state, mergedAM, name)
		if err != nil {
			return fmt.Errorf("opening merged map for %q: %w", name, err)
		}
		baseMap, err := collectionMapFromAM(ctx, state, baseAM, name)
		if err != nil {
			return fmt.Errorf("opening base map for %q: %w", name, err)
		}

		// Skip docs already in a data conflict on the initial pass (handled by
		// trigger 2); on a recheck validate them too, against the final validator.
		// existing dedupes against conflicts already recorded (any type).
		inDataConflict := make(map[string]struct{})
		existing := make(map[string]struct{}, len(allConflicts[name]))
		for _, e := range allConflicts[name] {
			existing[e.id] = struct{}{}
			if !recheck {
				inDataConflict[string(e.rawKey)] = struct{}{}
			}
		}

		var valEntries []*conflictEntry
		diffErr := forEachCollectionChange(ctx, baseMap, mergedMap, func(c collChange) (bool, error) {
			if c.kind == collRemoved {
				return false, nil // merged value absent -- nothing to validate
			}
			if _, dup := inDataConflict[string(c.key)]; dup {
				return false, nil // handled at resolution time (trigger 2)
			}
			mergedDoc, derr := readDocFromValue(ctx, state.ns, c.to)
			if derr != nil {
				return false, derr
			}
			conforms, verr := backends.DocumentSatisfiesValidator(mergedDoc, meta.Validator)
			if verr != nil {
				return false, verr
			}
			if conforms {
				return false, nil
			}
			entry := newValidationConflict(c, mergedDoc, meta.Validator, name, theirHash, theirsDesc)
			if _, dup := existing[entry.id]; dup {
				return false, nil // already recorded (e.g. resolved earlier)
			}
			// The merged value violates, and the action is "error" (warn was
			// skipped above). Every document the merge inserted or modified must
			// conform: a pre-existing violator is grandfathered only when the
			// merge leaves it byte-for-byte unchanged, and such a document is
			// never visited here (forEachCollectionChange skips unchanged docs).
			// A change that lands on a violating value -- even one-sided, even
			// when the base already violated -- is a conflict.
			valEntries = append(valEntries, entry)
			return false, nil
		})
		if diffErr != nil {
			return fmt.Errorf("cross-validating %q: %w", name, diffErr)
		}
		if len(valEntries) > 0 {
			allConflicts[name] = append(allConflicts[name], valEntries...)
		}
	}
	return nil
}

// recheckCrossValidation re-runs cross-validation at merge-continue for any
// collection whose validator definition conflicted and has now been resolved
// (workspace-h0w.5). Until resolution the collection's validator was unsettled,
// so the initial pass deferred it; now that the metaConflict is pinned, a
// document the merge changed into a state that violates the resolved validator
// must be caught. Returns true if new validation conflicts were recorded.
func (b *Backend) recheckCrossValidation(ctx context.Context, db *dbState, ms *mergeInProgress) (bool, error) {
	resolvedMeta := false
	for _, mc := range ms.metaConflicts {
		if mc.resolved {
			resolvedMeta = true
			break
		}
	}
	if !resolvedMeta {
		return false, nil
	}

	baseAM, err := b.mergeBaseAM(ctx, db, ms)
	if err != nil {
		return false, err
	}

	before := unresolvedConflictCount(ms.conflicts)
	theirsDesc := fmt.Sprintf("branch '%s' (theirs)", ms.fromBranch)
	if err := crossValidateMergedDocuments(ctx, db, ms.resolvedAM, baseAM, ms.conflicts, ms.metaConflicts, ms.fromHash, theirsDesc, true); err != nil {
		return false, err
	}
	return unresolvedConflictCount(ms.conflicts) > before, nil
}

// mergeBaseAM recomputes the merge base (common ancestor of the two branch
// HEADs the merge was started from) and returns its collections AddressMap.
func (b *Backend) mergeBaseAM(ctx context.Context, db *dbState, ms *mergeInProgress) (prolly.AddressMap, error) {
	intoCommit, err := datas.LoadCommitAddr(ctx, db.vs, ms.intoHash)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("mergeBaseAM: loading into commit: %w", err)
	}
	fromCommit, err := datas.LoadCommitAddr(ctx, db.vs, ms.fromHash)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("mergeBaseAM: loading from commit: %w", err)
	}
	baseHash, hasBase, err := datas.FindCommonAncestor(ctx, intoCommit, fromCommit, db.vs, db.vs, db.ns, db.ns)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("mergeBaseAM: finding common ancestor: %w", err)
	}
	if !hasBase {
		return prolly.AddressMap{}, fmt.Errorf("mergeBaseAM: no common ancestor")
	}
	return amFromCommitHash(ctx, db, baseHash.String())
}

// unresolvedConflictCount counts document/validation conflict entries not yet
// resolved across all collections.
func unresolvedConflictCount(conflicts map[string][]*conflictEntry) int {
	n := 0
	for _, entries := range conflicts {
		for _, e := range entries {
			if !e.resolved {
				n++
			}
		}
	}
	return n
}

// newValidationConflict builds a typ:"validation" conflict entry for a merged
// document that violates the collection validator. The offending merged document
// is carried in oursRawVal (surfaced as the current document) and the violated
// validator in reasonKey; there is no ours/theirs divergence -- resolution is
// replace-to-conform, not ours/theirs.
func newValidationConflict(c collChange, mergedDoc, validator *types.Document, coll string, theirHash hash.Hash, theirsDesc string) *conflictEntry {
	rawKey := append(val.Tuple(nil), c.key...)
	var idVal any = types.Null
	if v, err := mergedDoc.Get("_id"); err == nil {
		idVal = v
	}
	diff := "added"
	if c.kind == collModified {
		diff = "modified"
	}
	return &conflictEntry{
		id:            conflictID(rawKey, theirHash),
		typ:           "validation",
		rawKey:        rawKey,
		oursRawVal:    append(val.Tuple(nil), c.to...),
		ourDiffType:   diff,
		reasonCode:    "documentValidationFailure",
		reasonMessage: validationConflictMessage(idVal, coll, theirsDesc),
		reasonKey:     validator,
	}
}

func validationConflictMessage(id any, coll, theirsDesc string) string {
	return fmt.Sprintf("document %s in %q violates the collection validator merged from %s",
		types.FormatAnyValue(id), coll, theirsDesc)
}

// mergedCollValidator reads a collection's validator metadata from the resolved
// AddressMap of an in-progress merge (the validator that will be in effect once
// the merge completes).
func (b *Backend) mergedCollValidator(ctx context.Context, db *dbState, ms *mergeInProgress, coll string) (*collMeta, error) {
	catMap, err := catalogMapFromAM(ctx, db, ms.resolvedAM)
	if err != nil {
		return nil, err
	}
	return readCollMetaFromCatalog(ctx, db, catMap, coll)
}

// resolveValidationChoice computes the chosen value for a typ:"validation"
// conflict. Such a conflict has no ours/theirs divergence: the merged document
// violates the resulting validator, and the only way out is to replace it with a
// conforming value ("custom", re-validated here) or to drop it ("drop"). ours
// and theirs are rejected -- keeping a known violator is exactly the degradation
// the merge invariant forbids.
func (b *Backend) resolveValidationChoice(ctx context.Context, db *dbState, ms *mergeInProgress, target *conflictEntry, params *backends.ResolveConflictParams) (chosenVal val.Tuple, deleteDoc bool, err error) {
	switch params.Resolution {
	case "drop":
		return nil, true, nil
	case "custom":
		if params.Value == nil {
			return nil, false, fmt.Errorf("DumboDBResolveConflict: resolution \"custom\" requires a value document")
		}
		meta, merr := b.mergedCollValidator(ctx, db, ms, params.Collection)
		if merr != nil {
			return nil, false, merr
		}
		if meta != nil && meta.Validator != nil {
			ok, verr := backends.DocumentSatisfiesValidator(params.Value, meta.Validator)
			if verr != nil {
				return nil, false, verr
			}
			if !ok {
				return nil, false, fmt.Errorf("DumboDBResolveConflict: custom value still violates the collection validator for %q", params.Collection)
			}
		}
		v, werr := writeDocToValue(ctx, db.ns, params.Value)
		if werr != nil {
			return nil, false, fmt.Errorf("DumboDBResolveConflict: writing custom document: %w", werr)
		}
		return v, false, nil
	default:
		return nil, false, fmt.Errorf("DumboDBResolveConflict: validation conflict on %q resolves with 'custom' (a conforming replacement) or 'drop'; got %q", params.Collection, params.Resolution)
	}
}

// resolutionResultDoc returns the document that would remain after resolving a
// data conflict with params.Resolution, or nil when the resolution deletes the
// document (or supplies no value). Used for the trigger-2 check.
func resolutionResultDoc(ctx context.Context, db *dbState, target *conflictEntry, params *backends.ResolveConflictParams) (*types.Document, error) {
	switch params.Resolution {
	case "ours":
		if target.oursRawVal == nil {
			return nil, nil
		}
		return readDocFromValue(ctx, db.ns, target.oursRawVal)
	case "theirs":
		if target.theirsRawVal == nil {
			return nil, nil
		}
		return readDocFromValue(ctx, db.ns, target.theirsRawVal)
	case "custom":
		return params.Value, nil
	}
	return nil, nil
}

// rejectIfViolates fails a data-conflict resolution whose resulting document
// violates the collection's merged validator (trigger 2), unless the validator
// action is "warn". Resolving a conflict is an authoring act, so the authored
// value must conform regardless of the base state.
func (b *Backend) rejectIfViolates(ctx context.Context, db *dbState, ms *mergeInProgress, coll string, doc *types.Document) error {
	meta, err := b.mergedCollValidator(ctx, db, ms, coll)
	if err != nil {
		return err
	}
	if meta == nil || meta.Validator == nil || meta.ValidationLevel == "off" || meta.ValidationAction == "warn" {
		return nil
	}
	ok, verr := backends.DocumentSatisfiesValidator(doc, meta.Validator)
	if verr != nil {
		return verr
	}
	if !ok {
		return fmt.Errorf("DumboDBResolveConflict: resolved document for %q violates the collection validator; supply a conforming value ('custom') or drop it", coll)
	}
	return nil
}

// readCollMetaFromCatalog reads a single collection's metadata from an already
// opened catalog map, or nil when the collection has no catalog entry.
func readCollMetaFromCatalog(ctx context.Context, state *dbState, catMap prolly.Map, collName string) (*collMeta, error) {
	key, err := catalogKey(collName)
	if err != nil {
		return nil, err
	}
	var doc *types.Document
	if err := catMap.Get(ctx, key, func(_, v val.Tuple) error {
		if v == nil {
			return nil
		}
		d, derr := readBSONDocFromValue(ctx, state.ns, v)
		if derr != nil {
			return derr
		}
		doc = d
		return nil
	}); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, nil
	}
	return docToCollMeta(doc), nil
}
