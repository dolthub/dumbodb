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
	"reflect"
	"sort"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
)

// virtualFold is a merge of two merge bases into a virtual merge base. Where the merge would raise a conflict, the
// fold keeps a value both merge bases can be compared against and flags it instead.
type virtualFold struct {
	flags virtualBaseFlags
	// intoAM and fromAM are the merge bases, and baseAM their own merge base.
	intoAM, fromAM, baseAM prolly.AddressMap
	// reverted holds the documents the fold gave their value in baseAM to keep the virtual merge base valid.
	reverted map[string]map[string]bool
	// restored lists the collections the fold took from baseAM because one merge base deleted them.
	restored []string
	// touched holds, by collection, the documents the merge of the merge bases changed in some index.
	touched map[string][]val.Tuple
}

func newVirtualFold(intoAM, fromAM, baseAM prolly.AddressMap) *virtualFold {
	return &virtualFold{
		flags:    newVirtualBaseFlags(),
		intoAM:   intoAM,
		fromAM:   fromAM,
		baseAM:   baseAM,
		reverted: make(map[string]map[string]bool),
		touched:  make(map[string][]val.Tuple),
	}
}

func (f *virtualFold) touch(collection string, key val.Tuple) {
	f.touched[collection] = append(f.touched[collection], key)
}

func (f *virtualFold) flag(collection string, key val.Tuple, flag docFlag) {
	docs, ok := f.flags.docs[collection]
	if !ok {
		docs = make(map[string]docFlag)
		f.flags.docs[collection] = docs
	}
	docs[string(key)] = flag
}

// foldConflict writes the virtual merge base's value for a document its merge bases conflict on, keeps the indexes
// in step with it, and flags it.
func foldConflict(
	ctx context.Context,
	fold *virtualFold,
	collection string,
	diff tree.ThreeWayDiff,
	mode MergeMode,
	ns tree.NodeStore,
	mut *prolly.MutableMap,
	docOf func(val.Tuple) (*types.Document, error),
	applyIndexEdit func(mergeDocEdit, val.Tuple) error,
	indexing bool,
	inherited *docFlag,
) error {
	base, err := docOf(diff.Base)
	if err != nil {
		return err
	}
	left, err := docOf(diff.Left)
	if err != nil {
		return err
	}
	right, err := docOf(diff.Right)
	if err != nil {
		return err
	}
	key := append(val.Tuple(nil), diff.Key...)
	if collection == reservedCatalogName {
		// Catalog documents merge field by field. A collection both merge bases created starts from the fields
		// they agree on, so that the virtual merge base still has a catalog document for it.
		mode = MergeModeFieldDivergent
		if base == nil && left != nil && right != nil {
			base = types.MakeDocument(1)
			if id, ok := getOrNil(left, "_id"); ok {
				base.Set("_id", id)
			}
		}
	}
	value, flag := conflictValue(base, left, right, mode, inherited)
	if value == nil {
		if err := mut.Delete(ctx, key); err != nil {
			return fmt.Errorf("folding a conflict: %w", err)
		}
	} else {
		v, err := writeDocToValue(ctx, ns, value)
		if err != nil {
			return err
		}
		if err := mut.Put(ctx, key, v); err != nil {
			return fmt.Errorf("folding a conflict: %w", err)
		}
	}
	if indexing {
		if err := applyIndexEdit(mergeDocEdit{kind: editResolved, left: left, right: right, merged: value}, key); err != nil {
			return err
		}
	}
	if flag != nil {
		fold.flag(collection, key, *flag)
	}
	return nil
}

// conflictValue returns the document a virtual merge base keeps for a document its merge bases, |left| and |right|,
// conflict on in |mode|, and its flag, nil when it is not flagged. Any argument may be nil, meaning the document is
// absent on that side.
//
// It keeps the |base| document, flagged whole, except that:
//   - a document both merge bases added is left absent, as git leaves a file both sides added with conflict markers;
//   - a delete against a modify keeps the base document unflagged, as git does;
//   - in the field modes, fields the merge bases did not conflict on keep their merged value, and the flag lists the
//     field paths they conflicted on.
//
// |inherited| is the document's flag in the merge bases' own merge base, when that is a virtual merge base too: its
// paths count as changed on both sides.
func conflictValue(base, left, right *types.Document, mode MergeMode, inherited *docFlag) (*types.Document, *docFlag) {
	id := documentID(base, left, right)
	switch {
	case base == nil:
		return nil, &docFlag{id: id, whole: true}
	case left == nil && right == nil:
		return base, &docFlag{id: id, whole: true}
	case left == nil || right == nil:
		return base, nil
	case mode == MergeModeDocumentTouched || mode == MergeModeDocumentDivergent:
		return base, &docFlag{id: id, whole: true}
	case inherited != nil && inherited.whole:
		return base, &docFlag{id: id, whole: true}
	}
	paths := mode.conflictingPaths(base, left, right, nil)
	if inherited != nil {
		for _, path := range inherited.paths {
			if (mode == MergeModeFieldTouched || !pathsEqual(left, right, path)) && !containsPath(paths, path) {
				paths = append(paths, path)
			}
		}
	}
	merged, conflict := mergeBSONDoc(base, withBaseAt(left, base, paths), withBaseAt(right, base, paths))
	if conflict || len(paths) == 0 {
		return base, &docFlag{id: id, whole: true}
	}
	return merged, &docFlag{id: id, paths: paths}
}

// conflictingPaths returns the field paths, below |prefix|, that make |left| and |right| conflict against |base| in
// a field mode. It walks the documents as fieldConflicts does.
func (m MergeMode) conflictingPaths(base, left, right *types.Document, prefix []string) [][]string {
	var paths [][]string
	for _, key := range mergeDocumentKeys(base, left, right) {
		if prefix == nil && key == "_id" {
			continue
		}
		path := append(append([]string(nil), prefix...), key)
		baseValue, basePresent := getOrNil(base, key)
		leftValue, leftPresent := getOrNil(left, key)
		rightValue, rightPresent := getOrNil(right, key)
		baseDoc, leftDoc, rightDoc, documents := nestedMergeDocuments(baseValue, leftValue, rightValue)
		if documents {
			paths = append(paths, m.conflictingPaths(baseDoc, leftDoc, rightDoc, path)...)
			continue
		}
		leftChanged := basePresent != leftPresent || !reflect.DeepEqual(baseValue, leftValue)
		rightChanged := basePresent != rightPresent || !reflect.DeepEqual(baseValue, rightValue)
		if leftChanged && rightChanged && (m == MergeModeFieldTouched || !fieldsEqual(left, right, key)) {
			paths = append(paths, path)
		}
	}
	return paths
}

// withBaseAt returns a copy of |doc| whose values at |paths| are |base|'s, or absent where |base| has none.
func withBaseAt(doc, base *types.Document, paths [][]string) *types.Document {
	out := doc.DeepCopy()
	for _, path := range paths {
		parent, baseParent := out, base
		for _, key := range path[:len(path)-1] {
			next, _ := getOrNil(parent, key)
			parent, _ = next.(*types.Document)
			baseNext, _ := getOrNil(baseParent, key)
			baseParent, _ = baseNext.(*types.Document)
			if parent == nil {
				break
			}
		}
		if parent == nil {
			continue
		}
		last := path[len(path)-1]
		if value, ok := getOrNil(baseParent, last); ok {
			parent.Set(last, value)
		} else {
			parent.Remove(last)
		}
	}
	return out
}

func documentID(docs ...*types.Document) any {
	for _, doc := range docs {
		if id, ok := getOrNil(doc, "_id"); ok {
			return id
		}
	}
	return nil
}

// foldIndex is an index of a collection being folded, as it stands.
type foldIndex struct {
	info backends.IndexInfo
	m    prolly.Map
}

// probeLimit is how many owners of a unique key a probe reads. A larger group is resolved over several passes.
const probeLimit = 64

// makeValid makes collection |collection| of a virtual merge base, |merged|, as valid as a commit, given the merge
// bases' documents |into| and |from|, their own merge base's |base|, and the collection's |indexes|, which it updates
// in place. A document the fold changed that the collection's validator refuses, and a document that loses a unique
// key, keep their value in |base|, or are left absent when that value is no better; either way they are flagged.
//
// It reads and edits only the documents the fold changed: only they can fail the validator, and since both merge
// bases were valid, every document sharing a unique key shares it with one of them.
func (f *virtualFold) makeValid(ctx context.Context, state *dbState, collection string, merged, into, from, base prolly.Map, indexes []*foldIndex) (prolly.Map, error) {
	meta, err := f.validation(ctx, state, collection)
	if err != nil {
		return prolly.Map{}, err
	}
	ns := merged.NodeStore()
	edits := newFoldEdits(merged, indexes)
	var pending []val.Tuple
	if meta != nil && meta.Validator != nil && meta.ValidationLevel != "off" && meta.ValidationAction != "warn" {
		var refused []val.Tuple
		err := forEachCollectionChange(ctx, base, merged, func(c collChange) (bool, error) {
			if c.kind == collRemoved {
				return false, nil
			}
			doc, err := readDocFromValue(ctx, ns, c.to)
			if err != nil {
				return false, err
			}
			conforms, err := backends.DocumentSatisfiesValidator(doc, meta.Validator)
			if err != nil {
				return false, err
			}
			if !conforms {
				refused = append(refused, append(val.Tuple(nil), c.key...))
			}
			return false, nil
		})
		if err != nil {
			return prolly.Map{}, err
		}
		for _, key := range refused {
			if err := f.revert(ctx, ns, collection, edits, key, base); err != nil {
				return prolly.Map{}, err
			}
		}
		pending = append(pending, refused...)
	}

	// Each pass reverts at least one document, or leaves one absent, so this ends.
	pending = append(pending, f.touched[collection]...)
	for len(pending) > 0 {
		current, err := edits.flush(ctx)
		if err != nil {
			return prolly.Map{}, err
		}
		groups, err := uniqueCollisions(ctx, current, indexes, pending)
		if err != nil {
			return prolly.Map{}, err
		}
		pending = nil
		for _, group := range groups {
			losers, err := f.uniqueLosers(ctx, collection, group, current, into, from)
			if err != nil {
				return prolly.Map{}, err
			}
			for _, key := range losers {
				if err := f.revert(ctx, ns, collection, edits, key, base); err != nil {
					return prolly.Map{}, err
				}
			}
			pending = append(pending, losers...)
		}
	}
	return edits.flush(ctx)
}

// foldEdits edits a collection's documents and its indexes together.
type foldEdits struct {
	docs    *prolly.MutableMap
	indexes []*foldIndex
	muts    []*prolly.MutableMap
}

func newFoldEdits(docs prolly.Map, indexes []*foldIndex) *foldEdits {
	e := &foldEdits{docs: docs.Mutate(), indexes: indexes, muts: make([]*prolly.MutableMap, len(indexes))}
	for i, idx := range indexes {
		e.muts[i] = idx.m.Mutate()
	}
	return e
}

// replace gives document |key| the value |newValue|, nil to remove it, in the documents and in every index.
func (e *foldEdits) replace(ctx context.Context, key val.Tuple, oldDoc, newDoc *types.Document, newValue val.Tuple) error {
	if newValue == nil {
		if err := e.docs.Delete(ctx, key); err != nil {
			return err
		}
	} else if err := e.docs.Put(ctx, key, newValue); err != nil {
		return err
	}
	idBytes, ok := keyDesc.GetBytes(0, key)
	if !ok {
		return fmt.Errorf("primary key tuple missing _id bytes")
	}
	for i, idx := range e.indexes {
		if oldDoc != nil {
			rows, _, _ := indexEntriesForDoc(oldDoc, idx.info)
			for _, row := range rows {
				if err := idxpkg.DeleteEntry(ctx, e.muts[i], row, idBytes); err != nil {
					return err
				}
			}
		}
		if newDoc != nil {
			rows, multikey, lossy := indexEntriesForDoc(newDoc, idx.info)
			idx.info.Multikey = idx.info.Multikey || multikey
			idx.info.Lossy = idx.info.Lossy || lossy
			for _, row := range rows {
				if err := idxpkg.InsertEntry(ctx, e.muts[i], row, idBytes); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// flush returns the documents as they stand, and brings the indexes up to date.
func (e *foldEdits) flush(ctx context.Context) (prolly.Map, error) {
	docs, err := e.docs.Map(ctx)
	if err != nil {
		return prolly.Map{}, err
	}
	e.docs = docs.Mutate()
	for i, idx := range e.indexes {
		if idx.m, err = e.muts[i].Map(ctx); err != nil {
			return prolly.Map{}, err
		}
		e.muts[i] = idx.m.Mutate()
	}
	return docs, nil
}

// validation returns the collection's validation settings in the virtual merge base: the merge bases' when they
// agree or only one changed them, and their own merge base's when they conflict.
func (f *virtualFold) validation(ctx context.Context, state *dbState, collection string) (*collMeta, error) {
	ours, err := readCatalogDoc(ctx, state, f.intoAM, collection)
	if err != nil {
		return nil, err
	}
	theirs, err := readCatalogDoc(ctx, state, f.fromAM, collection)
	if err != nil {
		return nil, err
	}
	base, err := readCatalogDoc(ctx, state, f.baseAM, collection)
	if err != nil {
		return nil, err
	}
	switch {
	case validationSettingsEqual(ours, base):
		return theirs, nil
	case validationSettingsEqual(theirs, base) || validationSettingsEqual(ours, theirs):
		return ours, nil
	default:
		return base, nil
	}
}

// revert gives document |key| its value in |base|, or removes it when |base| has none, the document already has
// that value, or the fold already reverted it, and flags it whole.
func (f *virtualFold) revert(ctx context.Context, ns tree.NodeStore, collection string, edits *foldEdits, key val.Tuple, base prolly.Map) error {
	current, err := getDocument(ctx, ns, edits.docs, key)
	if err != nil {
		return err
	}
	baseValue, err := mapValue(ctx, base, key)
	if err != nil {
		return err
	}
	var baseDoc *types.Document
	if baseValue != nil {
		if baseDoc, err = readDocFromValue(ctx, ns, baseValue); err != nil {
			return err
		}
	}
	reverted := f.reverted[collection]
	if reverted == nil {
		reverted = make(map[string]bool)
		f.reverted[collection] = reverted
	}
	if baseDoc == nil || reverted[string(key)] || documentsEqual(current, baseDoc) {
		err = edits.replace(ctx, key, current, nil, nil)
	} else {
		err = edits.replace(ctx, key, current, baseDoc, baseValue)
	}
	if err != nil {
		return err
	}
	reverted[string(key)] = true
	f.flag(collection, key, docFlag{id: documentID(current, baseDoc), whole: true})
	return nil
}

// uniqueLosers returns the documents of |group|, which share a unique key, that give it up: the ones the fold already
// flagged, or else the ones whose value comes from the merge base folded later, |from|; or, when that does not single
// them out, all but the one with the lowest key.
func (f *virtualFold) uniqueLosers(ctx context.Context, collection string, group []val.Tuple, current, into, from prolly.Map) ([]val.Tuple, error) {
	var flagged, later []val.Tuple
	for _, key := range group {
		if _, ok := f.flags.docs[collection][string(key)]; ok {
			flagged = append(flagged, key)
		}
		currentValue, err := mapValue(ctx, current, key)
		if err != nil {
			return nil, err
		}
		fromValue, err := mapValue(ctx, from, key)
		if err != nil {
			return nil, err
		}
		intoValue, err := mapValue(ctx, into, key)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(currentValue, fromValue) && !bytes.Equal(currentValue, intoValue) {
			later = append(later, key)
		}
	}
	if len(flagged) > 0 && len(flagged) < len(group) {
		return flagged, nil
	}
	if len(later) > 0 && len(later) < len(group) {
		return later, nil
	}
	sorted := append([]val.Tuple(nil), group...)
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i], sorted[j]) < 0 })
	return sorted[1:], nil
}

// uniqueCollisions returns the groups of documents of |docs| that share a key of a unique index of |indexes| with a
// document of |keys|.
func uniqueCollisions(ctx context.Context, docs prolly.Map, indexes []*foldIndex, keys []val.Tuple) ([][]val.Tuple, error) {
	var groups [][]val.Tuple
	seen := make(map[string]bool)
	for _, key := range keys {
		doc, err := mapDocument(ctx, docs, key)
		if err != nil {
			return nil, err
		}
		if doc == nil {
			continue
		}
		for _, idx := range indexes {
			if !idx.info.Unique || idx.info.Lossy {
				continue
			}
			rows, _, lossy := indexEntriesForDoc(doc, idx.info)
			if lossy {
				continue
			}
			for _, row := range rows {
				start, stop := idxpkg.EqualityProbeBounds(row)
				ids, _, err := idxpkg.RangeLookupCapped(ctx, idx.m, start, stop, probeLimit)
				if err != nil {
					return nil, err
				}
				if len(ids) < 2 {
					continue
				}
				group := make([]val.Tuple, len(ids))
				for i, id := range ids {
					if group[i], err = buildKey(id); err != nil {
						return nil, err
					}
				}
				sort.Slice(group, func(i, j int) bool { return bytes.Compare(group[i], group[j]) < 0 })
				var id bytes.Buffer
				for _, k := range group {
					id.Write(k)
					id.WriteByte(0)
				}
				if !seen[id.String()] {
					seen[id.String()] = true
					groups = append(groups, group)
				}
			}
		}
	}
	return groups, nil
}

// foldIndexAM writes |indexes| as an index address map.
func foldIndexAM(ctx context.Context, state *dbState, indexes []*foldIndex) (prolly.AddressMap, error) {
	infos := make([]backends.IndexInfo, len(indexes))
	maps := make(map[string]prolly.Map, len(indexes))
	for i, idx := range indexes {
		infos[i] = idx.info
		maps[idx.info.Name] = idx.m
	}
	return buildIndexAM(ctx, state, infos, maps)
}

func mapDocument(ctx context.Context, m prolly.Map, key val.Tuple) (*types.Document, error) {
	value, err := mapValue(ctx, m, key)
	if err != nil || value == nil {
		return nil, err
	}
	return readDocFromValue(ctx, m.NodeStore(), value)
}

func mapValue(ctx context.Context, m prolly.Map, key val.Tuple) (val.Tuple, error) {
	var value val.Tuple
	err := m.Get(ctx, key, func(_, v val.Tuple) error {
		if v != nil {
			value = append(val.Tuple(nil), v...)
		}
		return nil
	})
	return value, err
}

func getDocument(ctx context.Context, ns tree.NodeStore, mut *prolly.MutableMap, key val.Tuple) (*types.Document, error) {
	var doc *types.Document
	err := mut.Get(ctx, key, func(_, v val.Tuple) error {
		if v == nil {
			return nil
		}
		var err error
		doc, err = readDocFromValue(ctx, ns, v)
		return err
	})
	return doc, err
}

// foldMergeMode returns the mode a fold merges collection |name| in when its merge bases' modes do not resolve
// three-way: their join in the lattice of merge modes.
func foldMergeMode(ctx context.Context, state *dbState, intoAM, fromAM prolly.AddressMap, name string) (MergeMode, error) {
	ours, err := readCatalogDoc(ctx, state, intoAM, name)
	if err != nil {
		return "", err
	}
	theirs, err := readCatalogDoc(ctx, state, fromAM, name)
	if err != nil {
		return "", err
	}
	return joinMergeModes(effectiveMetadataMergeMode(ours), effectiveMetadataMergeMode(theirs)), nil
}

// joinMergeModes returns the least strict merge mode at least as strict as both |a| and |b|. documentTouched is the
// strictest and fieldDivergent the least; fieldTouched and documentDivergent are incomparable.
func joinMergeModes(a, b MergeMode) MergeMode {
	switch {
	case a == b:
		return a
	case a == MergeModeFieldDivergent:
		return b
	case b == MergeModeFieldDivergent:
		return a
	default:
		return MergeModeDocumentTouched
	}
}

// restoreCatalogDocuments gives the collections |names| of |am| their catalog documents in |baseAM|.
func restoreCatalogDocuments(ctx context.Context, state *dbState, am, baseAM prolly.AddressMap, names []string) (prolly.AddressMap, error) {
	catalog, err := catalogMapFromAM(ctx, state, am)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	baseCatalog, err := catalogMapFromAM(ctx, state, baseAM)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	catalogH, err := am.Get(ctx, reservedCatalogName)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	idxAM, err := indexAMForDTBL(ctx, state.cs, state.ns, catalogH)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	resolved, err := indexSetFromAM(ctx, state, idxAM)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	var indexes []*foldIndex
	for _, entry := range resolved {
		m, err := openIndexMap(ctx, state.vs, state.ns, entry.mapRoot)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		indexes = append(indexes, &foldIndex{info: entry.info, m: m})
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].info.Name < indexes[j].info.Name })
	edits := newFoldEdits(catalog, indexes)
	for _, name := range names {
		key, err := catalogKey(name)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		value, err := mapValue(ctx, baseCatalog, key)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		if value == nil {
			continue
		}
		oldDoc, err := mapDocument(ctx, catalog, key)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		newDoc, err := readDocFromValue(ctx, state.ns, value)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		if err := edits.replace(ctx, key, oldDoc, newDoc, value); err != nil {
			return prolly.AddressMap{}, err
		}
	}
	restored, err := edits.flush(ctx)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	if idxAM, err = foldIndexAM(ctx, state, indexes); err != nil {
		return prolly.AddressMap{}, err
	}
	restoredH, err := state.dtblHashForCollection(ctx, reservedCatalogName, restored, idxAM, hash.Hash{})
	if err != nil {
		return prolly.AddressMap{}, err
	}
	editor := am.Editor()
	if err := editor.Update(ctx, reservedCatalogName, restoredH); err != nil {
		return prolly.AddressMap{}, err
	}
	return editor.Flush(ctx)
}

// flaggedDiff returns |diff| for a document its merge base flags with |flag|, or a conflict in its place. A flagged
// document counts as changed on both sides: the touched modes conflict on it, and the divergent modes when the sides
// differ on it, at its flagged paths or anywhere when it is flagged whole.
func flaggedDiff(ctx context.Context, diff tree.ThreeWayDiff, flag docFlag, mode MergeMode, baseMap prolly.Map, docOf func(val.Tuple) (*types.Document, error)) (tree.ThreeWayDiff, error) {
	if diff.Op == tree.DiffOpDivergentModifyConflict || diff.Op == tree.DiffOpDivergentDeleteConflict {
		return diff, nil
	}
	baseValue, err := mapValue(ctx, baseMap, diff.Key)
	if err != nil {
		return diff, err
	}
	left, right := diff.Left, diff.Right
	switch diff.Op {
	case tree.DiffOpLeftAdd, tree.DiffOpLeftModify, tree.DiffOpLeftDelete:
		right = baseValue
	case tree.DiffOpRightAdd, tree.DiffOpRightModify, tree.DiffOpRightDelete:
		left = baseValue
	case tree.DiffOpConvergentAdd, tree.DiffOpConvergentModify, tree.DiffOpConvergentDelete:
		right = left
	}
	conflict := mode == MergeModeDocumentTouched || mode == MergeModeFieldTouched
	if !conflict {
		leftDoc, err := docOf(left)
		if err != nil {
			return diff, err
		}
		rightDoc, err := docOf(right)
		if err != nil {
			return diff, err
		}
		if flag.whole {
			conflict = !documentsEqual(leftDoc, rightDoc)
		}
		for _, path := range flag.paths {
			conflict = conflict || !pathsEqual(leftDoc, rightDoc, path)
		}
	}
	if !conflict {
		return diff, nil
	}
	op := tree.DiffOpDivergentModifyConflict
	if left == nil || right == nil {
		op = tree.DiffOpDivergentDeleteConflict
	}
	return tree.ThreeWayDiff{Op: op, Key: diff.Key, Base: baseValue, Left: left, Right: right}, nil
}

// pathsEqual reports whether |left| and |right| hold the same value at |path|, or both none.
func pathsEqual(left, right *types.Document, path []string) bool {
	leftValue, leftOK := pathValue(left, path)
	rightValue, rightOK := pathValue(right, path)
	return leftOK == rightOK && reflect.DeepEqual(leftValue, rightValue)
}

func pathValue(doc *types.Document, path []string) (any, bool) {
	for _, key := range path[:len(path)-1] {
		next, ok := getOrNil(doc, key)
		if !ok {
			return nil, false
		}
		if doc, ok = next.(*types.Document); !ok {
			return nil, false
		}
	}
	return getOrNil(doc, path[len(path)-1])
}

func containsPath(paths [][]string, path []string) bool {
	for _, p := range paths {
		if reflect.DeepEqual(p, path) {
			return true
		}
	}
	return false
}
