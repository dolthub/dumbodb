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
	"reflect"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/types"
)

// MergeMode names what makes two branches' changes to one document a conflict.
// Touched means a side wrote it at all; Divergent means the two sides wrote it
// differently. The unit is the whole document or an individual field.
//
// See docs/design/merge-strictness.md.
type MergeMode string

const (
	MergeModeDocumentTouched   MergeMode = "documentTouched"
	MergeModeFieldTouched      MergeMode = "fieldTouched"
	MergeModeFieldDivergent    MergeMode = "fieldDivergent"
	MergeModeDocumentDivergent MergeMode = "documentDivergent"
)

// DefaultMergeMode is what a collection gets when it declares nothing. The
// MongoDB compare-and-swap pattern has to work without opting in, and
// fieldTouched is the weakest mode under which it does.
const DefaultMergeMode = MergeModeFieldTouched

func (m MergeMode) valid() bool {
	switch m {
	case MergeModeDocumentTouched, MergeModeFieldTouched,
		MergeModeFieldDivergent, MergeModeDocumentDivergent:
		return true
	}
	return false
}

// changedFields reports the top-level keys where doc differs from base. A nil
// base is an insert, so every key of doc is new; a nil doc is a delete, so
// every key that existed changed.
//
// _id is excluded. It is the document's identity, not a field anyone chose to
// write: it is present in every version by construction and equal on both
// sides whenever this is the same document. Counting it would make two sides
// adding the same _id collide even when the fields they actually wrote are
// disjoint, which includes concurrent implicit collection creation.
func changedFields(base, doc *types.Document) map[string]struct{} {
	changed := map[string]struct{}{}
	switch {
	case base == nil:
		if doc != nil {
			for _, k := range doc.Keys() {
				changed[k] = struct{}{}
			}
		}
	case doc == nil:
		for _, k := range base.Keys() {
			changed[k] = struct{}{}
		}
	default:
		keys := map[string]struct{}{}
		for _, k := range base.Keys() {
			keys[k] = struct{}{}
		}
		for _, k := range doc.Keys() {
			keys[k] = struct{}{}
		}
		for k := range keys {
			bVal, bOK := getOrNil(base, k)
			dVal, dOK := getOrNil(doc, k)
			if bOK != dOK || !reflect.DeepEqual(bVal, dVal) {
				changed[k] = struct{}{}
			}
		}
	}
	delete(changed, "_id")
	return changed
}

func fieldsEqual(left, right *types.Document, key string) bool {
	lVal, lOK := getOrNil(left, key)
	rVal, rOK := getOrNil(right, key)
	return lOK == rOK && reflect.DeepEqual(lVal, rVal)
}

func documentsEqual(left, right *types.Document) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if len(left.Keys()) != len(right.Keys()) {
		return false
	}
	for _, k := range left.Keys() {
		if !fieldsEqual(left, right, k) {
			return false
		}
	}
	return true
}

// conflicts applies the mode to one three-way document decision. Any argument
// may be nil, meaning the document is absent on that side.
func (m MergeMode) conflicts(base, left, right *types.Document) bool {
	// A delete is only reconcilable against another delete. Both sides
	// removing the document is agreement about the result, which the Touched
	// modes still refuse, because they refuse to read agreement as permission.
	if left == nil || right == nil {
		if left == nil && right == nil {
			return m == MergeModeDocumentTouched || m == MergeModeFieldTouched
		}
		return true
	}

	fo := changedFields(base, left)
	ft := changedFields(base, right)
	if len(fo) == 0 || len(ft) == 0 {
		return false
	}

	switch m {
	case MergeModeDocumentTouched:
		return true
	case MergeModeFieldTouched:
		for k := range fo {
			if _, ok := ft[k]; ok {
				return true
			}
		}
		return false
	case MergeModeFieldDivergent:
		for k := range fo {
			if _, ok := ft[k]; !ok {
				continue
			}
			if !fieldsEqual(left, right, k) {
				return true
			}
		}
		return false
	case MergeModeDocumentDivergent:
		return !documentsEqual(left, right)
	}
	return true
}

// rowMergePolicy adapts a merge mode to the callback Dolt's merge consults for
// every three-way row decision, including the convergent ones it would
// otherwise resolve without asking. That is the whole point: two
// compare-and-swap updates that agree on the resulting value are convergent,
// and fieldTouched has to refuse them.
func rowMergePolicy(ns tree.NodeStore, mode MergeMode) tree.RowMergePolicy {
	if !mode.valid() {
		mode = DefaultMergeMode
	}
	return func(sqlCtx *sql.Context, left, right, base val.Tuple) (val.Tuple, tree.RowMergeStatus, error) {
		var ctx context.Context = context.Background()
		if sqlCtx != nil {
			ctx = sqlCtx
		}
		leftDoc, err := documentOrNil(ctx, ns, left)
		if err != nil {
			return nil, tree.RowMergeDefer, nil
		}
		rightDoc, err := documentOrNil(ctx, ns, right)
		if err != nil {
			return nil, tree.RowMergeDefer, nil
		}
		baseDoc, err := documentOrNil(ctx, ns, base)
		if err != nil {
			return nil, tree.RowMergeDefer, nil
		}

		if mode.conflicts(baseDoc, leftDoc, rightDoc) {
			return nil, tree.RowMergeConflict, nil
		}

		// Not a conflict under this mode, so the merged document has to be
		// produced here: with the whole document in one cell, deferring would
		// hand the decision back to a rule that compares whole values.
		if leftDoc == nil && rightDoc == nil {
			return nil, tree.RowMergeResolved, nil
		}
		if baseDoc == nil {
			baseDoc = types.MakeDocument(0)
		}
		merged, conflict := mergeBSONDoc(baseDoc, leftDoc, rightDoc)
		if conflict {
			return nil, tree.RowMergeConflict, nil
		}
		mergedVal, err := writeDocToValue(ctx, ns, merged)
		if err != nil {
			return nil, tree.RowMergeDefer, nil
		}
		return mergedVal, tree.RowMergeResolved, nil
	}
}

// documentOrNil decodes a stored value tuple, resolving a document held out of
// band. A nil tuple means the document is absent on that side.
func documentOrNil(ctx context.Context, ns tree.NodeStore, v val.Tuple) (*types.Document, error) {
	if v == nil || len(v) == 0 {
		return nil, nil
	}
	return readDocFromValue(ctx, ns, v)
}
