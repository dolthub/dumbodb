// Copyright 2023 Dolthub, Inc.
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

// Three-way JSON document merge. Adapted from
// dolt/go/libraries/doltcore/merge to avoid pulling in the full SQL engine.

import (
	"bytes"
	"context"
	"io"

	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
)

// threeWayJsonDiffer iterates over three-way diffs between JSON documents.
type threeWayJsonDiffer struct {
	leftDiffer, rightDiffer           tree.JsonDiffer
	leftCurrentDiff, rightCurrentDiff *tree.JsonDiff
	leftIsDone, rightIsDone           bool
	ns                                tree.NodeStore
}

type threeWayJsonDiff struct {
	Op                  tree.DiffOp
	Key                 []byte
	Left, Right, Merged sql.JSONWrapper
}

func (differ *threeWayJsonDiffer) Next(ctx context.Context) (threeWayJsonDiff, error) {
	for {
		err := differ.loadNextDiff(ctx)
		if err != nil {
			return threeWayJsonDiff{}, err
		}

		if differ.rightIsDone {
			return threeWayJsonDiff{}, io.EOF
		}

		if differ.leftIsDone {
			return differ.processRightSideOnlyDiff(), nil
		}

		leftDiff := differ.leftCurrentDiff
		rightDiff := differ.rightCurrentDiff
		leftKey := leftDiff.Key
		rightKey := rightDiff.Key

		cmp := bytes.Compare(leftKey, rightKey)
		if cmp != 0 && tree.JsonKeysModifySameArray(leftKey, rightKey) {
			result := threeWayJsonDiff{Op: tree.DiffOpDivergentModifyConflict}
			return result, nil
		}
		if cmp > 0 {
			if tree.IsJsonKeyPrefix(leftKey, rightKey) {
				result := threeWayJsonDiff{Op: tree.DiffOpDivergentModifyConflict}
				differ.leftCurrentDiff = nil
				return result, nil
			}
			return differ.processRightSideOnlyDiff(), nil
		} else if cmp < 0 {
			if tree.IsJsonKeyPrefix(rightKey, leftKey) {
				result := threeWayJsonDiff{Op: tree.DiffOpDivergentModifyConflict}
				differ.rightCurrentDiff = nil
				return result, nil
			}
			differ.leftCurrentDiff = nil
			continue
		} else {
			// Both diffs are on the same key.
			if differ.leftCurrentDiff.From == nil {
				valueCmp, err := types.CompareJSON(ctx, differ.leftCurrentDiff.To, differ.rightCurrentDiff.To)
				if err != nil {
					return threeWayJsonDiff{}, err
				}
				if valueCmp == 0 {
					return differ.processMergedDiff(tree.DiffOpConvergentAdd, differ.leftCurrentDiff.To), nil
				}
				return differ.processMergedDiff(tree.DiffOpDivergentModifyConflict, nil), nil
			}
			if differ.leftCurrentDiff.To == nil && differ.rightCurrentDiff.To == nil {
				return differ.processMergedDiff(tree.DiffOpConvergentDelete, nil), nil
			}
			if differ.leftCurrentDiff.To == nil || differ.rightCurrentDiff.To == nil {
				return differ.processMergedDiff(tree.DiffOpDivergentDeleteConflict, nil), nil
			}
			mergedValue, conflict, err := mergeJSON(ctx, differ.ns, differ.leftCurrentDiff.From,
				differ.leftCurrentDiff.To, differ.rightCurrentDiff.To)
			if err != nil {
				return threeWayJsonDiff{}, err
			}
			if conflict {
				return differ.processMergedDiff(tree.DiffOpDivergentModifyConflict, nil), nil
			}
			return differ.processMergedDiff(tree.DiffOpDivergentModifyResolved, mergedValue), nil
		}
	}
}

func (differ *threeWayJsonDiffer) loadNextDiff(ctx context.Context) error {
	if differ.leftCurrentDiff == nil && !differ.leftIsDone {
		newLeftDiff, err := differ.leftDiffer.Next(ctx)
		if err == io.EOF {
			differ.leftIsDone = true
		} else if err != nil {
			return err
		} else {
			differ.leftCurrentDiff = &newLeftDiff
		}
	}
	if differ.rightCurrentDiff == nil && !differ.rightIsDone {
		newRightDiff, err := differ.rightDiffer.Next(ctx)
		if err == io.EOF {
			differ.rightIsDone = true
		} else if err != nil {
			return err
		} else {
			differ.rightCurrentDiff = &newRightDiff
		}
	}
	return nil
}

func (differ *threeWayJsonDiffer) processRightSideOnlyDiff() threeWayJsonDiff {
	switch differ.rightCurrentDiff.Type {
	case tree.AddedDiff:
		result := threeWayJsonDiff{Op: tree.DiffOpRightAdd, Key: differ.rightCurrentDiff.Key, Right: differ.rightCurrentDiff.To}
		differ.rightCurrentDiff = nil
		return result
	case tree.RemovedDiff:
		result := threeWayJsonDiff{Op: tree.DiffOpRightDelete, Key: differ.rightCurrentDiff.Key}
		differ.rightCurrentDiff = nil
		return result
	case tree.ModifiedDiff:
		result := threeWayJsonDiff{Op: tree.DiffOpRightModify, Key: differ.rightCurrentDiff.Key, Right: differ.rightCurrentDiff.To}
		differ.rightCurrentDiff = nil
		return result
	default:
		panic("unreachable")
	}
}

func (differ *threeWayJsonDiffer) processMergedDiff(op tree.DiffOp, merged sql.JSONWrapper) threeWayJsonDiff {
	result := threeWayJsonDiff{
		Op: op, Key: differ.leftCurrentDiff.Key,
		Left: differ.leftCurrentDiff.To, Right: differ.rightCurrentDiff.To, Merged: merged,
	}
	differ.leftCurrentDiff = nil
	differ.rightCurrentDiff = nil
	return result
}

// mergeJSON performs a three-way merge of JSON documents. Non-overlapping
// field changes are merged automatically; overlapping changes produce a
// conflict (conflict=true).
func mergeJSON(ctx context.Context, ns tree.NodeStore, baseJson, leftJson, rightJson sql.JSONWrapper) (resultDoc sql.JSONWrapper, conflict bool, err error) {
	baseIsObject, err := tree.IsJsonObject(ctx, baseJson)
	if err != nil {
		return nil, true, err
	}
	leftIsObject, err := tree.IsJsonObject(ctx, leftJson)
	if err != nil {
		return nil, true, err
	}
	rightIsObject, err := tree.IsJsonObject(ctx, rightJson)
	if err != nil {
		return nil, true, err
	}

	if !baseIsObject || !leftIsObject || !rightIsObject {
		cmp, err := types.CompareJSON(ctx, leftJson, rightJson)
		if err != nil {
			return types.JSONDocument{}, true, err
		}
		if cmp == 0 {
			return leftJson, false, nil
		}
		return types.JSONDocument{}, true, nil
	}

	leftDiffer, err := tree.NewJsonDiffer(ctx, baseJson, leftJson)
	if err != nil {
		return nil, true, err
	}

	rightDiffer, err := tree.NewJsonDiffer(ctx, baseJson, rightJson)
	if err != nil {
		return nil, true, err
	}

	differ := threeWayJsonDiffer{
		leftDiffer:  leftDiffer,
		rightDiffer: rightDiffer,
		ns:          ns,
	}

	var ok bool
	var merged tree.IndexedJsonDocument
	if merged, ok = leftJson.(tree.IndexedJsonDocument); !ok {
		root, err := tree.SerializeJsonToAddr(ctx, ns, leftJson)
		if err != nil {
			return types.JSONDocument{}, true, err
		}
		merged = tree.NewIndexedJsonDocument(ctx, root, ns)
	}

	for {
		threeWayDiff, err := differ.Next(ctx)
		if err == io.EOF {
			return merged, false, nil
		}
		if err != nil {
			return types.JSONDocument{}, true, err
		}

		switch threeWayDiff.Op {
		case tree.DiffOpRightAdd, tree.DiffOpConvergentAdd, tree.DiffOpRightModify, tree.DiffOpConvergentModify, tree.DiffOpDivergentModifyResolved:
			merged, _, err = merged.SetWithKey(ctx, threeWayDiff.Key, threeWayDiff.Right)
			if err != nil {
				return types.JSONDocument{}, true, err
			}
		case tree.DiffOpRightDelete, tree.DiffOpConvergentDelete:
			merged, _, err = merged.RemoveWithKey(ctx, threeWayDiff.Key)
			if err != nil {
				return types.JSONDocument{}, true, err
			}
		case tree.DiffOpLeftAdd, tree.DiffOpLeftModify, tree.DiffOpLeftDelete:
			// already in left
		case tree.DiffOpDivergentModifyConflict, tree.DiffOpDivergentDeleteConflict:
			return types.JSONDocument{}, true, nil
		default:
			panic("unreachable")
		}
	}
}
