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
	"encoding/hex"
	"fmt"

	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// collMetaToBSONHex / collMetaFromBSONHex persist a per-collection metadata doc
// in the merge-state file (self-contained, no chunk-store dependency), mirroring
// the view-conflict persistence.
func collMetaToBSONHex(coll string, m *collMeta) (string, error) {
	doc, err := collMetaToDoc(coll, m)
	if err != nil {
		return "", err
	}
	stored, err := docToBSON(doc)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(stored), nil
}

func collMetaFromBSONHex(s string) (*collMeta, error) {
	stored, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	doc, err := bsonToDoc(stored)
	if err != nil {
		return nil, err
	}
	return docToCollMeta(doc), nil
}

// metaConflictEntry is a collection-metadata (validator/options) merge conflict:
// the same collection's durable metadata diverged on both branches. It is
// surfaced on the OWNING collection -- the internal __dumbo_catalog__ name is
// never exposed. base/ours/theirs are the per-collection metadata on each side
// (nil where that side had no metadata for the collection).
type metaConflictEntry struct {
	coll      string
	id        string
	base      *collMeta
	ours      *collMeta
	theirs    *collMeta
	ourDiff   string // "added", "modified", "deleted"
	theirDiff string
	resolved  bool
}

// metaConflictsFromCatalog converts divergent __dumbo_catalog__ document
// conflicts into per-owning-collection metadata conflicts. Each catalog doc's
// _id is its collection name, so the conflict is re-keyed onto that collection.
func metaConflictsFromCatalog(ctx context.Context, state *dbState, conflicts []*conflictEntry) (map[string]*metaConflictEntry, error) {
	decode := func(v val.Tuple) (*collMeta, string, error) {
		if v == nil {
			return nil, "", nil
		}
		doc, err := readBSONDocFromValue(ctx, state.ns, v)
		if err != nil {
			return nil, "", err
		}
		name := ""
		if id, e := doc.Get("_id"); e == nil {
			name, _ = id.(string)
		}
		return docToCollMeta(doc), name, nil
	}

	out := map[string]*metaConflictEntry{}
	for _, c := range conflicts {
		baseMeta, bn, err := decode(c.baseRawVal)
		if err != nil {
			return nil, err
		}
		oursMeta, on, err := decode(c.oursRawVal)
		if err != nil {
			return nil, err
		}
		theirsMeta, tn, err := decode(c.theirsRawVal)
		if err != nil {
			return nil, err
		}
		coll := firstNonEmpty(on, tn, bn)
		if coll == "" {
			continue
		}
		out[coll] = &metaConflictEntry{
			coll:      coll,
			id:        c.id,
			base:      baseMeta,
			ours:      oursMeta,
			theirs:    theirsMeta,
			ourDiff:   c.ourDiffType,
			theirDiff: c.theirDiffType,
		}
	}
	return out, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// metaFromResolveValue builds the collMeta for a "custom" metadata resolution:
// it starts from an existing side (to preserve the collection's UUID and any
// timeseries fields, which the user does not restate) and overrides the
// user-facing validator / validationLevel / validationAction from the supplied
// value document.
func metaFromResolveValue(value *types.Document, mce *metaConflictEntry) *collMeta {
	var m collMeta
	switch {
	case mce.ours != nil:
		m = *mce.ours
	case mce.base != nil:
		m = *mce.base
	case mce.theirs != nil:
		m = *mce.theirs
	}
	if v, err := value.Get("validator"); err == nil {
		if vd, ok := v.(*types.Document); ok {
			m.Validator = vd
		} else {
			m.Validator = nil
		}
	}
	if v, err := value.Get("validationLevel"); err == nil {
		if s, ok := v.(string); ok {
			m.ValidationLevel = s
		}
	}
	if v, err := value.Get("validationAction"); err == nil {
		if s, ok := v.(string); ok {
			m.ValidationAction = s
		}
	}
	return &m
}

// resolveMetaConflict resolves a single collection-metadata merge conflict by
// choosing ours, theirs, or a custom definition, rewriting the collection's
// metadata in the resolved __dumbo_catalog__. The caller holds db.mu (write
// lock). Mirrors resolveViewConflict.
func (b *Backend) resolveMetaConflict(ctx context.Context, db *dbState, ms *mergeInProgress, mce *metaConflictEntry, params *backends.ResolveConflictParams) (*backends.ResolveConflictResult, error) {
	if mce.resolved {
		return nil, fmt.Errorf("DumboDBResolveConflict: metadata conflict for %q is already resolved", params.Collection)
	}

	// "ours" is already reflected in resolvedAM (the catalog doc kept ours during
	// capture); just mark resolved.
	if params.Resolution == "ours" {
		mce.resolved = true
		return &backends.ResolveConflictResult{}, nil
	}

	var newMeta *collMeta // nil => the resolution deletes the collection's metadata
	switch params.Resolution {
	case "theirs":
		newMeta = mce.theirs
	case "custom":
		if params.Value == nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: resolution %q requires a value document", params.Resolution)
		}
		newMeta = metaFromResolveValue(params.Value, mce)
	default:
		return nil, fmt.Errorf("DumboDBResolveConflict: unknown resolution %q (must be 'ours', 'theirs', or 'custom')", params.Resolution)
	}

	editor := ms.resolvedAM.Editor()
	if newMeta == nil {
		if err := db.applyCatalogDelete(ctx, ms.resolvedAM, editor, mce.coll); err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: deleting metadata for %q: %w", mce.coll, err)
		}
	} else {
		if err := db.applyCatalogUpsert(ctx, ms.resolvedAM, editor, mce.coll, newMeta); err != nil {
			return nil, fmt.Errorf("DumboDBResolveConflict: writing resolved metadata for %q: %w", mce.coll, err)
		}
	}

	finalAM, err := editor.Flush(ctx)
	if err != nil {
		return nil, fmt.Errorf("DumboDBResolveConflict: flushing AM editor: %w", err)
	}

	ms.resolvedAM = finalAM
	mce.resolved = true

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
