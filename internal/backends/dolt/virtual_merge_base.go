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
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"

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
}

func newVirtualFold(intoAM, fromAM, baseAM prolly.AddressMap) *virtualFold {
	return &virtualFold{
		flags:    make(virtualBaseFlags),
		intoAM:   intoAM,
		fromAM:   fromAM,
		baseAM:   baseAM,
		reverted: make(map[string]map[string]bool),
	}
}

func (f *virtualFold) flag(collection string, key val.Tuple, flag docFlag) {
	docs, ok := f.flags[collection]
	if !ok {
		docs = make(map[string]docFlag)
		f.flags[collection] = docs
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
	value, flag := conflictValue(base, left, right, mode)
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
func conflictValue(base, left, right *types.Document, mode MergeMode) (*types.Document, *docFlag) {
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
	}
	paths := mode.conflictingPaths(base, left, right, nil)
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

// makeValid makes collection |collection| of a virtual merge base, |merged|, as valid as a commit, given the merge
// bases' documents |into| and |from|, their own merge base's |base|, and the collection's indexes. A document the fold
// changed that the collection's validator refuses, and a document that loses a unique key, keep their value in |base|,
// or are left absent when that value is no better; either way they are flagged.
func (f *virtualFold) makeValid(ctx context.Context, state *dbState, collection string, merged, into, from, base prolly.Map, infos []backends.IndexInfo) (prolly.Map, error) {
	meta, err := f.validation(ctx, state, collection)
	if err != nil {
		return prolly.Map{}, err
	}
	mut := merged.Mutate()
	ns := merged.NodeStore()
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
			if err := f.revert(ctx, ns, collection, mut, key, base); err != nil {
				return prolly.Map{}, err
			}
		}
	}

	// Each pass reverts at least one document, or leaves one absent, so this ends.
	for {
		current, err := mut.Map(ctx)
		if err != nil {
			return prolly.Map{}, err
		}
		groups, err := uniqueCollisions(ctx, current, infos)
		if err != nil {
			return prolly.Map{}, err
		}
		if len(groups) == 0 {
			return current, nil
		}
		for _, group := range groups {
			losers, err := f.uniqueLosers(ctx, collection, group, current, into, from)
			if err != nil {
				return prolly.Map{}, err
			}
			for _, key := range losers {
				if err := f.revert(ctx, ns, collection, mut, key, base); err != nil {
					return prolly.Map{}, err
				}
			}
		}
	}
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
func (f *virtualFold) revert(ctx context.Context, ns tree.NodeStore, collection string, mut *prolly.MutableMap, key val.Tuple, base prolly.Map) error {
	current, err := getDocument(ctx, ns, mut, key)
	if err != nil {
		return err
	}
	var baseValue val.Tuple
	if err := base.Get(ctx, key, func(_, v val.Tuple) error {
		if v != nil {
			baseValue = append(val.Tuple(nil), v...)
		}
		return nil
	}); err != nil {
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
		if err := mut.Delete(ctx, key); err != nil {
			return err
		}
	} else if err := mut.Put(ctx, key, baseValue); err != nil {
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
		if _, ok := f.flags[collection][string(key)]; ok {
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

// uniqueCollisions returns the groups of documents of |docs| that share a key of a unique index of |infos|.
func uniqueCollisions(ctx context.Context, docs prolly.Map, infos []backends.IndexInfo) ([][]val.Tuple, error) {
	var groups [][]val.Tuple
	for _, info := range infos {
		if !info.Unique || info.Lossy {
			continue
		}
		owners := make(map[string][]val.Tuple)
		var prefixes []string
		err := eachDocument(ctx, docs, func(key val.Tuple, doc *types.Document) error {
			rows, _, lossy := indexEntriesForDoc(doc, info)
			if lossy {
				return nil
			}
			seen := make(map[string]bool)
			for _, row := range rows {
				prefix, _ := idxpkg.EqualityProbeBounds(row)
				if seen[string(prefix)] {
					continue
				}
				seen[string(prefix)] = true
				if owners[string(prefix)] == nil {
					prefixes = append(prefixes, string(prefix))
				}
				owners[string(prefix)] = append(owners[string(prefix)], append(val.Tuple(nil), key...))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, prefix := range prefixes {
			if len(owners[prefix]) > 1 {
				groups = append(groups, owners[prefix])
			}
		}
	}
	return groups, nil
}

// buildIndexesFromDocuments builds the indexes |infos| over |docs|.
func buildIndexesFromDocuments(ctx context.Context, state *dbState, docs prolly.Map, infos []backends.IndexInfo) (prolly.AddressMap, error) {
	maps := make(map[string]prolly.Map, len(infos))
	built := make([]backends.IndexInfo, len(infos))
	for i, info := range infos {
		empty, err := idxpkg.NewEmptyMap(ctx, state.ns)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		mut := empty.Mutate()
		err = eachDocument(ctx, docs, func(key val.Tuple, doc *types.Document) error {
			idBytes, ok := keyDesc.GetBytes(0, key)
			if !ok {
				return fmt.Errorf("primary key tuple missing _id bytes")
			}
			rows, multikey, lossy := indexEntriesForDoc(doc, info)
			info.Multikey = info.Multikey || multikey
			info.Lossy = info.Lossy || lossy
			for _, row := range rows {
				if err := idxpkg.InsertEntry(ctx, mut, row, idBytes); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return prolly.AddressMap{}, err
		}
		if maps[info.Name], err = mut.Map(ctx); err != nil {
			return prolly.AddressMap{}, err
		}
		built[i] = info
	}
	return buildIndexAM(ctx, state, built, maps)
}

func eachDocument(ctx context.Context, docs prolly.Map, fn func(key val.Tuple, doc *types.Document) error) error {
	iter, err := docs.IterAll(ctx)
	if err != nil {
		return err
	}
	for {
		k, v, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		doc, err := readDocFromValue(ctx, docs.NodeStore(), v)
		if err != nil {
			return err
		}
		if err := fn(k, doc); err != nil {
			return err
		}
	}
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
