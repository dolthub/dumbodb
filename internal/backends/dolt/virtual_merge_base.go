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
	"reflect"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/types"
)

// virtualFold is a merge of two merge bases into a virtual merge base. Where the merge would raise a conflict, the
// fold keeps a value both merge bases can be compared against and flags it instead.
type virtualFold struct {
	flags virtualBaseFlags
}

func newVirtualFold() *virtualFold {
	return &virtualFold{flags: make(virtualBaseFlags)}
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
