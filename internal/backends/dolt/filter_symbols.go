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
	"slices"
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
)

// predicateKind is how mongod's planner can use an index for a predicate.
type predicateKind int

const (
	predicateEquality predicateKind = iota
	predicateRange
	predicateNegation
	predicateExprEquality
	predicateExprRange
)

// symbolPredicate is one comparison of a field against an operand.
type symbolPredicate struct {
	field   string
	kind    predicateKind
	operand any
	// literal is the operand as a filter states it, for mongod's rule on
	// indexes of another collation.
	literal any
}

// symbolCheck holds what checkFilterSymbols needs besides the filter.
type symbolCheck struct {
	ctx            context.Context
	idxInfos       []backends.IndexInfo
	root           *types.Document
	queryCollation *types.Document
	queryIdentity  string
	hinted         string
}

// checkFilterSymbols returns the error mongod gives a filter that compares a
// field with a Symbol when an index with a collation on that field is a
// candidate for the query: its planner cannot build that index's bounds.
func checkFilterSymbols(ctx context.Context, idxInfos []backends.IndexInfo, filter, queryCollation *types.Document, hint any) error {
	if filter == nil || !valueHasSymbol(filter) {
		return nil
	}
	sc := &symbolCheck{
		ctx:            ctx,
		idxInfos:       idxInfos,
		root:           filter,
		queryCollation: queryCollation,
		queryIdentity:  collation.Parse(queryCollation).Identity(),
	}
	if hint != nil {
		if backends.HintIsNatural(hint) {
			return nil
		}
		sc.hinted = backends.MatchHintedIndex(hint, idxInfos)
	}
	return sc.checkConjunction(filter, false)
}

// checkFilterSymbols is checkFilterSymbols over c's indexes.
func (c *collection) checkFilterSymbols(ctx context.Context, state *dbState, filter, queryCollation *types.Document, hint any) error {
	if filter == nil || !valueHasSymbol(filter) {
		return nil
	}
	idxInfos, _, err := resolveIndexesLocked(ctx, c, state)
	if err != nil {
		return err
	}
	return checkFilterSymbols(ctx, idxInfos, filter, queryCollation, hint)
}

// checkConjunction checks the predicates of one AND level; negated marks the
// branches of a $nor.
func (sc *symbolCheck) checkConjunction(filter *types.Document, negated bool) error {
	var predicates []symbolPredicate
	fields := map[string]bool{}
	keys, values := filter.Keys(), filter.Values()
	for i, k := range keys {
		switch k {
		case "$and", "$or", "$nor":
			branches, _ := values[i].(*types.Array)
			for j := 0; branches != nil && j < branches.Len(); j++ {
				branch, _ := branches.Get(j)
				if doc, ok := branch.(*types.Document); ok {
					if err := sc.checkConjunction(doc, negated || k == "$nor"); err != nil {
						return err
					}
				}
			}
		case "$expr":
			for _, p := range exprPredicates(values[i]) {
				fields[p.field] = true
				predicates = append(predicates, p)
			}
		default:
			if strings.HasPrefix(k, "$") {
				continue
			}
			fields[k] = true
			for _, p := range fieldPredicates(k, values[i]) {
				if negated {
					p.kind = predicateNegation
				}
				predicates = append(predicates, p)
			}
		}
	}
	for _, p := range predicates {
		if !valueHasSymbol(p.operand) {
			continue
		}
		for _, idx := range sc.idxInfos {
			if !sc.isCandidate(idx, p, fields) {
				continue
			}
			if err := idxpkg.CheckCollatableQuery(p.field[strings.LastIndexByte(p.field, '.')+1:], p.operand); err != nil {
				return err
			}
		}
	}
	return nil
}

// isCandidate reports whether mongod's planner would build idx's bounds for p,
// given the fields the same AND level constrains.
func (sc *symbolCheck) isCandidate(idx backends.IndexInfo, p symbolPredicate, fields map[string]bool) bool {
	if idx.Hidden || indexComparator(idx) == nil || len(idx.Key) == 0 {
		return false
	}
	if sc.hinted != "" && idx.Name != sc.hinted {
		return false
	}
	pos := slices.IndexFunc(idx.Key, func(kp backends.IndexKeyPair) bool { return kp.Field == p.field })
	if pos < 0 || !fields[idx.Key[0].Field] {
		return false
	}
	if idx.Key[pos].Text || isGeoKey(idx.Key[pos]) {
		return false
	}
	switch p.kind {
	case predicateNegation:
		if idx.Sparse || idx.Key[pos].Hashed {
			return false
		}
	case predicateRange, predicateExprRange:
		if idx.Key[pos].Hashed {
			return false
		}
	}
	if (p.kind == predicateExprEquality || p.kind == predicateExprRange) && idx.Sparse {
		return false
	}
	if idx.PartialFilterExpression != nil && !partialIndexServes(sc.ctx, sc.root, idx, sc.queryCollation) {
		return false
	}
	if collation.Parse(idx.Collation).Identity() != sc.queryIdentity && !comparesNoStringObjectOrArray(p.literal) {
		return false
	}
	return true
}

// fieldPredicates splits a field's filter value into its comparisons.
func fieldPredicates(field string, v any) []symbolPredicate {
	doc, ok := v.(*types.Document)
	if !ok || doc.Len() == 0 || !strings.HasPrefix(doc.Keys()[0], "$") {
		return []symbolPredicate{{field: field, kind: predicateEquality, operand: v, literal: v}}
	}
	var out []symbolPredicate
	ops, operands := doc.Keys(), doc.Values()
	for i, op := range ops {
		kind := predicateEquality
		switch op {
		case "$ne", "$nin", "$not":
			kind = predicateNegation
		case "$gt", "$gte", "$lt", "$lte", "$elemMatch":
			kind = predicateRange
		}
		out = append(out, symbolPredicate{field: field, kind: kind, operand: operands[i], literal: v})
	}
	return out
}

// exprPredicates returns the comparisons of a field with a constant that
// mongod rewrites an $expr into, so an index can bound them.
func exprPredicates(expr any) []symbolPredicate {
	doc, ok := expr.(*types.Document)
	if !ok || doc.Len() != 1 {
		return nil
	}
	op := doc.Keys()[0]
	args, _ := doc.Values()[0].(*types.Array)
	if args == nil {
		return nil
	}
	if op == "$and" {
		var out []symbolPredicate
		for i := range args.Len() {
			arg, _ := args.Get(i)
			out = append(out, exprPredicates(arg)...)
		}
		return out
	}
	kind := predicateExprRange
	switch op {
	case "$eq":
		kind = predicateExprEquality
	case "$gt", "$gte", "$lt", "$lte":
	default:
		return nil
	}
	if args.Len() != 2 {
		return nil
	}
	a, _ := args.Get(0)
	b, _ := args.Get(1)
	if path, ok := exprFieldPath(a); ok {
		return []symbolPredicate{{field: path, kind: kind, operand: b, literal: b}}
	}
	if path, ok := exprFieldPath(b); ok {
		return []symbolPredicate{{field: path, kind: kind, operand: a, literal: a}}
	}
	return nil
}

func exprFieldPath(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "$") || strings.HasPrefix(s, "$$") {
		return "", false
	}
	return s[1:], true
}

// filterBindsSymbol reports whether filter compares a key field of idx with
// a Symbol.
func filterBindsSymbol(filter *types.Document, idx backends.IndexInfo) bool {
	for _, kp := range idx.Key {
		if v, err := filter.Get(kp.Field); err == nil && valueHasSymbol(v) {
			return true
		}
	}
	return false
}

func valueHasSymbol(v any) bool {
	switch v := v.(type) {
	case types.Symbol:
		return true
	case *types.Document:
		for _, fv := range v.Values() {
			if valueHasSymbol(fv) {
				return true
			}
		}
	case *types.Array:
		for i := range v.Len() {
			if ev, _ := v.Get(i); valueHasSymbol(ev) {
				return true
			}
		}
	}
	return false
}

// comparesNoStringObjectOrArray reports whether a field's predicate compares
// against no string, regex, object or array.
func comparesNoStringObjectOrArray(v any) bool {
	switch v := v.(type) {
	case string, types.Regex, *types.Array:
		return false
	case *types.Document:
		if v.Len() == 0 || !strings.HasPrefix(v.Keys()[0], "$") {
			return false
		}
		for _, operand := range v.Values() {
			if list, ok := operand.(*types.Array); ok {
				for i := range list.Len() {
					if el, _ := list.Get(i); !comparesNoStringObjectOrArray(el) {
						return false
					}
				}
				continue
			}
			if !comparesNoStringObjectOrArray(operand) {
				return false
			}
		}
	}
	return true
}
