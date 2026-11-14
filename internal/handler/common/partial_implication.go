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

package common

import (
	"context"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// QueryImpliesPartialFilter reports whether query implies a partial index's
// filter expression pfe by mongod's subset rules (expression::isSubsetOf),
// which decide whether a hinted scan of the index takes query's bounds or
// reads the whole index. known is false for a pfe shape it does not model.
//
// Unlike the backend's check for choosing an index, this follows mongod
// exactly rather than erring toward a collection scan. It returns ctx's error
// once ctx is done.
func QueryImpliesPartialFilter(ctx context.Context, query, pfe *types.Document) (implied, known bool, err error) {
	rhs := parseSubsetExpr(pfe)
	if !rhs.modeledAsPartialFilter() {
		return false, false, nil
	}
	c := &subsetChecker{ctx: ctx}
	implied = c.implies(parseSubsetExpr(query), rhs)
	if c.err != nil {
		return false, false, c.err
	}
	return implied, true, nil
}

// subsetCheckInterval is how many steps of implies run between checks of the
// context.
const subsetCheckInterval = 1024

type subsetKind int

const (
	subsetLeaf subsetKind = iota
	subsetAnd
	subsetOr
	subsetOther
)

// subsetExpr is a filter as mongod's subset rules see it: conjunctions,
// disjunctions and single-operator predicates on a path. A conjunction also
// holds its leaf children by path and the indexes of the others, so a leaf
// finds the children that can imply it without visiting the rest.
type subsetExpr struct {
	kind     subsetKind
	children []subsetExpr
	path     string
	op       string
	value    any
	leaves   map[string][]int
	compound []int
}

func parseSubsetExpr(filter *types.Document) subsetExpr {
	and := subsetExpr{kind: subsetAnd}
	keys, values := filter.Keys(), filter.Values()
	for i, k := range keys {
		v := values[i]
		switch {
		case k == "$and" || k == "$or":
			node := subsetExpr{kind: subsetAnd}
			if k == "$or" {
				node.kind = subsetOr
			}
			for _, b := range filterBranches(v) {
				node.children = append(node.children, parseSubsetExpr(b))
			}
			node.indexChildren()
			and.children = append(and.children, node)
		case k == "$nor":
			and.children = append(and.children, parseNor(v))
		case strings.HasPrefix(k, "$"):
			and.children = append(and.children, subsetExpr{kind: subsetOther})
		default:
			and.children = append(and.children, parseFieldCondition(k, v)...)
		}
	}
	and.indexChildren()
	return and
}

func (e *subsetExpr) indexChildren() {
	e.leaves = make(map[string][]int)
	for i, c := range e.children {
		if c.kind == subsetLeaf {
			e.leaves[c.path] = append(e.leaves[c.path], i)
		} else {
			e.compound = append(e.compound, i)
		}
	}
}

// parseNor reads a $nor of one single-field branch as that branch's negation,
// as mongod's canonical form does; any other $nor is opaque.
func parseNor(v any) subsetExpr {
	branches := filterBranches(v)
	if len(branches) != 1 || branches[0].Len() != 1 || strings.HasPrefix(branches[0].Keys()[0], "$") {
		return subsetExpr{kind: subsetOther}
	}
	path := branches[0].Keys()[0]
	return subsetExpr{kind: subsetLeaf, path: path, op: "$not", value: must.NotFail(branches[0].Get(path))}
}

func parseFieldCondition(path string, v any) []subsetExpr {
	ops, ok := v.(*types.Document)
	if !ok || ops.Len() == 0 || !strings.HasPrefix(ops.Keys()[0], "$") {
		return []subsetExpr{{kind: subsetLeaf, path: path, op: "$eq", value: v}}
	}
	opKeys, opValues := ops.Keys(), ops.Values()
	leaves := make([]subsetExpr, 0, len(opKeys))
	for i, op := range opKeys {
		leaves = append(leaves, subsetExpr{kind: subsetLeaf, path: path, op: op, value: opValues[i]})
	}
	return leaves
}

// modeledAsPartialFilter reports whether e is a conjunction of predicates
// whose subsets implies decides.
func (e subsetExpr) modeledAsPartialFilter() bool {
	switch e.kind {
	case subsetAnd:
		for _, c := range e.children {
			if !c.modeledAsPartialFilter() {
				return false
			}
		}
		return true
	case subsetLeaf:
		switch e.op {
		case "$eq", "$gt", "$gte", "$lt", "$lte":
			return true
		case "$exists":
			return existsOperandTrue(e.value)
		}
	}
	return false
}

// subsetChecker runs implies, stopping with ctx's error once ctx is done.
type subsetChecker struct {
	ctx   context.Context
	steps int
	err   error
}

// implies follows mongod's order: every conjunct of rhs, then any conjunct
// of e, then every branch of e, then any branch of rhs.
func (c *subsetChecker) implies(e, rhs subsetExpr) bool {
	if c.err != nil {
		return false
	}
	if c.steps%subsetCheckInterval == 0 {
		if c.err = c.ctx.Err(); c.err != nil {
			return false
		}
	}
	c.steps++
	if rhs.kind == subsetAnd {
		for _, r := range rhs.children {
			if !c.implies(e, r) {
				return false
			}
		}
		return true
	}
	switch e.kind {
	case subsetAnd:
		// Only a leaf on rhs's path, or a compound child, can imply a leaf.
		if rhs.kind == subsetLeaf {
			for _, i := range e.leaves[rhs.path] {
				if c.implies(e.children[i], rhs) {
					return true
				}
			}
			for _, i := range e.compound {
				if c.implies(e.children[i], rhs) {
					return true
				}
			}
			return false
		}
		for _, child := range e.children {
			if c.implies(child, rhs) {
				return true
			}
		}
		return false
	case subsetOr:
		if len(e.children) == 0 {
			return false
		}
		for _, child := range e.children {
			if !c.implies(child, rhs) {
				return false
			}
		}
		return true
	case subsetLeaf:
		return rhs.kind == subsetLeaf && e.path == rhs.path && e.leafImplies(rhs)
	}
	return false
}

func (e subsetExpr) leafImplies(rhs subsetExpr) bool {
	if rhs.op == "$exists" {
		return e.excludesMissing()
	}
	switch e.op {
	case "$eq", "$gt", "$gte", "$lt", "$lte":
		return comparisonWithin(e.op, e.value, rhs.op, rhs.value)
	case "$in":
		values, ok := e.value.(*types.Array)
		if !ok || values.Len() == 0 {
			return false
		}
		for i := 0; i < values.Len(); i++ {
			if !comparisonWithin("$eq", must.NotFail(values.Get(i)), rhs.op, rhs.value) {
				return false
			}
		}
		return true
	case "$all":
		values, ok := e.value.(*types.Array)
		if !ok {
			return false
		}
		for i := 0; i < values.Len(); i++ {
			if comparisonWithin("$eq", must.NotFail(values.Get(i)), rhs.op, rhs.value) {
				return true
			}
		}
	}
	return false
}

// excludesMissing reports whether mongod deems predicate e unable to match a
// document missing its path. Comparisons count unless against null, even
// against MinKey or MaxKey; of negations only those of null count.
func (e subsetExpr) excludesMissing() bool {
	switch e.op {
	case "$eq", "$gt", "$gte", "$lt", "$lte":
		return e.value != types.Null
	case "$in":
		values, ok := e.value.(*types.Array)
		return ok && !values.Contains(types.Null)
	case "$exists":
		return existsOperandTrue(e.value)
	case "$type", "$regex", "$mod", "$size", "$elemMatch", "$all":
		return true
	case "$ne":
		return e.value == types.Null
	case "$nin":
		values, ok := e.value.(*types.Array)
		return ok && values.Contains(types.Null)
	case "$not":
		return negatesNull(e.value)
	}
	return false
}

// negatesNull reports whether a $not or $nor operand matches null, so its
// negation cannot match a missing field.
func negatesNull(v any) bool {
	ops, ok := v.(*types.Document)
	if !ok || ops.Len() == 0 || !strings.HasPrefix(ops.Keys()[0], "$") {
		return v == types.Null
	}
	if ops.Len() != 1 {
		return false
	}
	operand := must.NotFail(ops.Get(ops.Keys()[0]))
	switch ops.Keys()[0] {
	case "$eq":
		return operand == types.Null
	case "$in":
		values, ok := operand.(*types.Array)
		return ok && values.Contains(types.Null)
	}
	return false
}

// comparisonWithin reports whether every value lop lv matches is matched by
// rop rv: both in one canonical type bracket, lv's range inside rv's.
func comparisonWithin(lop string, lv any, rop string, rv any) bool {
	if typeBracket(lv) != typeBracket(rv) {
		return false
	}
	c := types.CompareOrder(lv, rv, types.Ascending)
	switch rop {
	case "$eq":
		return lop == "$eq" && c == types.Equal
	case "$gte":
		return (lop == "$eq" || lop == "$gte" || lop == "$gt") && c != types.Less
	case "$gt":
		switch lop {
		case "$eq", "$gte":
			return c == types.Greater
		case "$gt":
			return c != types.Less
		}
	case "$lte":
		return (lop == "$eq" || lop == "$lte" || lop == "$lt") && c != types.Greater
	case "$lt":
		switch lop {
		case "$eq", "$lte":
			return c == types.Less
		case "$lt":
			return c != types.Greater
		}
	}
	return false
}

// typeBracket is v's canonical type for comparisons; numbers, NaN included,
// share one.
func typeBracket(v any) string {
	switch v.(type) {
	case int32, int64, float64, types.Decimal128:
		return "number"
	case string:
		return "string"
	case *types.Document:
		return "object"
	case *types.Array:
		return "array"
	case types.Binary:
		return "binData"
	case types.ObjectID:
		return "objectId"
	case bool:
		return "bool"
	case types.Timestamp:
		return "timestamp"
	case types.Regex:
		return "regex"
	case types.NullType:
		return "null"
	case types.MinKeyType:
		return "minKey"
	case types.MaxKeyType:
		return "maxKey"
	case time.Time:
		return "date"
	}
	return "other"
}
