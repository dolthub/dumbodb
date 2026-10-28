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

package operators

import (
	"errors"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type fieldSpecLeaf int

const (
	fieldSpecSubtree fieldSpecLeaf = iota
	fieldSpecInclude
	fieldSpecExclude
	fieldSpecExpr
)

// fieldSpecNode is one field of a projection or $addFields specification,
// with dotted keys and nested specification objects merged into one tree.
type fieldSpecNode struct {
	leaf     fieldSpecLeaf
	expr     any
	keys     []string
	children map[string]*fieldSpecNode
	hasExpr  bool
}

// buildFieldSpec builds the tree of spec; projection selects projection
// semantics, where numbers and booleans include or exclude.
func buildFieldSpec(spec *types.Document, projection bool) *fieldSpecNode {
	root := &fieldSpecNode{children: map[string]*fieldSpecNode{}}
	root.add(spec, projection)
	root.markExprs()
	return root
}

func (n *fieldSpecNode) add(spec *types.Document, projection bool) {
	for _, key := range spec.Keys() {
		node := n
		for _, part := range strings.Split(key, ".") {
			child, ok := node.children[part]
			if !ok {
				child = &fieldSpecNode{children: map[string]*fieldSpecNode{}}
				node.children[part] = child
				node.keys = append(node.keys, part)
			}
			node = child
		}
		value := must.NotFail(spec.Get(key))
		if nested, ok := value.(*types.Document); ok && !IsOperator(nested) && nested.Len() > 0 {
			node.add(nested, projection)
			continue
		}
		node.leaf, node.expr = fieldSpecExpr, value
		if projection {
			switch v := value.(type) {
			case bool:
				node.leaf = fieldSpecExclude
				if v {
					node.leaf = fieldSpecInclude
				}
			case int32, int64, float64:
				node.leaf = fieldSpecExclude
				if types.Compare(v, int32(0)) != types.Equal {
					node.leaf = fieldSpecInclude
				}
			}
		}
	}
}

func (n *fieldSpecNode) markExprs() bool {
	n.hasExpr = n.leaf == fieldSpecExpr
	for _, child := range n.children {
		if child.markExprs() {
			n.hasExpr = true
		}
	}
	return n.hasExpr
}

// SpecHasComputedPath reports whether spec computes a field below the top
// level, through a dotted key or a nested specification object.
func SpecHasComputedPath(spec *types.Document, projection bool) bool {
	root := buildFieldSpec(spec, projection)
	for _, key := range root.keys {
		child := root.children[key]
		if child.leaf == fieldSpecSubtree && child.hasExpr {
			return true
		}
	}
	return false
}

// evalComputed evaluates a computed field's expression against root, reporting
// whether it is missing.
func evalComputed(expr any, root *types.Document) (any, bool, error) {
	switch v := expr.(type) {
	case *types.Document:
		if !IsOperator(v) {
			return v, false, nil
		}
		op, err := NewOperator(v)
		if err != nil {
			return nil, false, err
		}
		res, err := op.Process(root)
		if errors.Is(err, ErrMissingValue) {
			return nil, true, nil
		}
		return res, false, err
	case *types.Array:
		res, err := EvalArrayExpression(v, root)
		return res, false, err
	case string:
		if !strings.HasPrefix(v, "$") {
			return v, false, nil
		}
		res, missing, err := evalArgValueOrMissing(v, root)
		if errors.Is(err, ErrMissingValue) {
			return nil, true, nil
		}
		return res, missing, err
	default:
		return v, false, nil
	}
}

// EvalArrayExpression evaluates each element of an array literal as an
// expression; a missing field becomes null.
func EvalArrayExpression(arr *types.Array, doc *types.Document) (*types.Array, error) {
	out := types.MakeArray(arr.Len())
	for i := 0; i < arr.Len(); i++ {
		v := must.NotFail(arr.Get(i))
		if nested, ok := v.(*types.Array); ok {
			evaluated, err := EvalArrayExpression(nested, doc)
			if err != nil {
				return nil, err
			}
			out.Append(evaluated)
			continue
		}
		evaluated, err := EvalArgValue(v, doc)
		if errors.Is(err, ErrMissingValue) {
			evaluated, err = types.Null, nil
		}
		if err != nil {
			return nil, err
		}
		out.Append(evaluated)
	}
	return out, nil
}

// ProjectInclusionTree applies an inclusion projection spec that computes
// fields below the top level: included fields keep their document order,
// computed fields follow in spec order, and _id comes first unless excluded.
func ProjectInclusionTree(doc, spec *types.Document) (*types.Document, error) {
	root := buildFieldSpec(spec, true)
	if _, ok := root.children["_id"]; !ok {
		root.children["_id"] = &fieldSpecNode{leaf: fieldSpecInclude}
		root.keys = append([]string{"_id"}, root.keys...)
	}
	res, _, err := root.project(doc, true, doc)
	if err != nil {
		return nil, err
	}
	out := res.(*types.Document)
	if id, err := out.Get("_id"); err == nil && out.Keys()[0] != "_id" {
		out.Remove("_id")
		reordered := must.NotFail(types.NewDocument("_id", id))
		for _, k := range out.Keys() {
			reordered.Set(k, must.NotFail(out.Get(k)))
		}
		out = reordered
	}
	out.SetRecordID(doc.RecordID())
	return out, nil
}

func (n *fieldSpecNode) project(val any, present bool, root *types.Document) (any, bool, error) {
	if present {
		switch v := val.(type) {
		case *types.Document:
			return n.projectDocument(v, root)
		case *types.Array:
			out := types.MakeArray(v.Len())
			for i := 0; i < v.Len(); i++ {
				el := must.NotFail(v.Get(i))
				switch el.(type) {
				case *types.Document, *types.Array:
					res, ok, err := n.project(el, true, root)
					if err != nil {
						return nil, false, err
					}
					if ok {
						out.Append(res)
					}
				default:
					if n.hasExpr {
						res, _, err := n.projectDocument(types.MakeDocument(0), root)
						if err != nil {
							return nil, false, err
						}
						out.Append(res)
					}
				}
			}
			return out, true, nil
		}
	}
	if !n.hasExpr {
		return nil, false, nil
	}
	return n.projectDocument(types.MakeDocument(0), root)
}

func (n *fieldSpecNode) projectDocument(v, root *types.Document) (any, bool, error) {
	out := types.MakeDocument(0)
	for _, key := range v.Keys() {
		child, ok := n.children[key]
		if !ok {
			continue
		}
		switch child.leaf {
		case fieldSpecInclude:
			out.Set(key, must.NotFail(v.Get(key)))
		case fieldSpecSubtree:
			res, ok, err := child.project(must.NotFail(v.Get(key)), true, root)
			if err != nil {
				return nil, false, err
			}
			if ok {
				out.Set(key, res)
			}
		}
	}
	for _, key := range n.keys {
		child := n.children[key]
		switch {
		case child.leaf == fieldSpecExpr:
			res, missing, err := evalComputed(child.expr, root)
			if err != nil {
				return nil, false, err
			}
			if !missing {
				out.Set(key, res)
			}
		case child.leaf == fieldSpecSubtree && child.hasExpr && !v.Has(key):
			res, ok, err := child.project(nil, false, root)
			if err != nil {
				return nil, false, err
			}
			if ok {
				out.Set(key, res)
			}
		}
	}
	return out, true, nil
}

// AddFieldsTree applies an $addFields specification to doc: computed fields
// are set at their paths, merging into the documents found there and creating
// them where a path holds no document. A missing value removes the field.
func AddFieldsTree(doc, spec *types.Document) (*types.Document, error) {
	root := buildFieldSpec(spec, false)
	res, err := root.addFields(doc, doc)
	if err != nil {
		return nil, err
	}
	res.SetRecordID(doc.RecordID())
	return res, nil
}

func (n *fieldSpecNode) addFields(v, root *types.Document) (*types.Document, error) {
	for _, key := range n.keys {
		child := n.children[key]
		if child.leaf == fieldSpecExpr {
			res, missing, err := evalComputed(child.expr, root)
			if err != nil {
				return nil, err
			}
			if missing {
				v.Remove(key)
			} else {
				v.Set(key, res)
			}
			continue
		}
		cur, err := v.Get(key)
		res, err := child.addFieldsValue(cur, err == nil, root)
		if err != nil {
			return nil, err
		}
		v.Set(key, res)
	}
	return v, nil
}

func (n *fieldSpecNode) addFieldsValue(val any, present bool, root *types.Document) (any, error) {
	if present {
		switch v := val.(type) {
		case *types.Document:
			return n.addFields(v.DeepCopy(), root)
		case *types.Array:
			out := types.MakeArray(v.Len())
			for i := 0; i < v.Len(); i++ {
				res, err := n.addFieldsValue(must.NotFail(v.Get(i)), true, root)
				if err != nil {
					return nil, err
				}
				out.Append(res)
			}
			return out, nil
		}
	}
	return n.addFields(types.MakeDocument(0), root)
}
