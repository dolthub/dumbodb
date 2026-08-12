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

// crossValidateMergedDocuments emits a typ:"validation" conflict for every
// document a merge changed into a validator-violating state (action "error"),
// or logs one per-collection summary under action "warn".
//
// recheck=false is the initial pass: unresolved-metaConflict collections and
// data-conflict docs are skipped. recheck=true re-runs ONLY collections whose
// metaConflict is now resolved and validates every changed document (no
// data-conflict skip). Entries whose conflictID already exists are never
// re-added.
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
				continue
			}
		} else if hasMeta && !mc.resolved {
			continue
		}

		meta, err := readCollMetaFromCatalog(ctx, state, catMap, name)
		if err != nil {
			return fmt.Errorf("reading merged validator for %q: %w", name, err)
		}
		if meta == nil || meta.Validator == nil || meta.ValidationLevel == "off" {
			continue
		}
		warn := meta.ValidationAction == "warn"

		mergedMap, err := collectionMapFromAM(ctx, state, mergedAM, name)
		if err != nil {
			return fmt.Errorf("opening merged map for %q: %w", name, err)
		}
		baseMap, err := collectionMapFromAM(ctx, state, baseAM, name)
		if err != nil {
			return fmt.Errorf("opening base map for %q: %w", name, err)
		}

		inDataConflict := make(map[string]struct{})
		existing := make(map[string]struct{}, len(allConflicts[name]))
		for _, e := range allConflicts[name] {
			existing[e.id] = struct{}{}
			if !recheck {
				inDataConflict[string(e.rawKey)] = struct{}{}
			}
		}

		var valEntries []*conflictEntry
		var warnCount int32
		diffErr := forEachCollectionChange(ctx, baseMap, mergedMap, func(c collChange) (bool, error) {
			if c.kind == collRemoved {
				return false, nil
			}
			if _, dup := inDataConflict[string(c.key)]; dup {
				return false, nil
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
			if warn {
				warnCount++
				return false, nil
			}
			entry := newValidationConflict(c, mergedDoc, meta.Validator, name, theirHash, theirsDesc)
			if _, dup := existing[entry.id]; dup {
				return false, nil
			}
			valEntries = append(valEntries, entry)
			return false, nil
		})
		if diffErr != nil {
			return fmt.Errorf("cross-validating %q: %w", name, diffErr)
		}
		if warn {
			if warnCount > 0 && state.backend != nil && state.backend.l != nil {
				state.backend.l.Warn("documents allowed despite failing validation during merge (validationAction:warn)",
					"collection", name, "count", warnCount)
			}
			continue
		}
		if len(valEntries) > 0 {
			allConflicts[name] = append(allConflicts[name], valEntries...)
		}
	}
	return nil
}

// recheckCrossValidation re-runs cross-validation at merge-continue for any
// collection whose validator conflict has now been resolved. Returns true if
// new validation conflicts were recorded.
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
	theirsDesc := fmt.Sprintf("%s (theirs)", refLabel(ctx, db, ms.fromBranch))
	if err := crossValidateMergedDocuments(ctx, db, ms.resolvedAM, baseAM, ms.conflicts, ms.metaConflicts, ms.fromHash, theirsDesc, true); err != nil {
		return false, err
	}
	return unresolvedConflictCount(ms.conflicts) > before, nil
}

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

// newValidationConflict builds a typ:"validation" conflict entry. There is no
// ours/theirs divergence: the offending merged document is carried in oursRawVal
// and the violated validator in reasonKey.
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

func (b *Backend) mergedCollValidator(ctx context.Context, db *dbState, ms *mergeInProgress, coll string) (*collMeta, error) {
	catMap, err := catalogMapFromAM(ctx, db, ms.resolvedAM)
	if err != nil {
		return nil, err
	}
	return readCollMetaFromCatalog(ctx, db, catMap, coll)
}

// resolveValidationChoice computes the chosen value for a typ:"validation"
// conflict: "custom" (re-validated here) or "drop". ours and theirs are rejected.
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
// data conflict, or nil when the resolution deletes the document or supplies no
// value.
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
// violates the collection's merged validator, unless the validator action is
// "warn".
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

// readCollMetaFromCatalog reads a collection's metadata from an already-opened
// catalog map; nil when the collection has no catalog entry.
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
