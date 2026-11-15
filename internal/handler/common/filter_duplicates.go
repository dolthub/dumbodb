// Copyright 2021 FerretDB Inc.
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
	"slices"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// NormalizeDuplicateFilterKeys rewrites, in place, the filters of a command
// that repeat a key into an equivalent form without repeated keys, which is
// safe to read: types.Document.Get panics on a repeated key. mongod reads
// repeated filter keys as an implicit $and. The filters are those of find,
// count, distinct, delete, update, findAndModify and aggregate $match stages,
// also under explain. Repeats it cannot rewrite are left for the command's
// duplicate field check to refuse.
func NormalizeDuplicateFilterKeys(cmd *types.Document) {
	if cmd == nil {
		return
	}
	switch cmd.Command() {
	case "find":
		normalizeFilterField(cmd, "filter")
	case "count", "distinct", "findAndModify", "findandmodify":
		normalizeFilterField(cmd, "query")
	case "delete":
		normalizeStatementFilters(cmd, "deletes")
	case "update":
		normalizeStatementFilters(cmd, "updates")
	case "aggregate":
		if pipeline, ok := uniqueField(cmd, "pipeline").(*types.Array); ok {
			normalizePipelineFilters(pipeline)
		}
	case "explain":
		if inner, ok := uniqueField(cmd, "explain").(*types.Document); ok {
			NormalizeDuplicateFilterKeys(inner)
		}
	}
}

// uniqueField returns the value of doc's field key, or nil when doc lacks it
// or repeats it.
func uniqueField(doc *types.Document, key string) any {
	var found any
	values := doc.Values()
	for i, k := range doc.Keys() {
		if k != key {
			continue
		}
		if found != nil {
			return nil
		}
		found = values[i]
	}
	return found
}

func normalizeFilterField(doc *types.Document, key string) {
	filter, ok := uniqueField(doc, key).(*types.Document)
	if !ok {
		return
	}
	if normalized := normalizeFilter(filter); normalized != filter {
		doc.Set(key, normalized)
	}
}

func normalizeStatementFilters(cmd *types.Document, key string) {
	statements, ok := uniqueField(cmd, key).(*types.Array)
	if !ok {
		return
	}
	for i := range statements.Len() {
		if statement, ok := must.NotFail(statements.Get(i)).(*types.Document); ok {
			normalizeFilterField(statement, "q")
		}
	}
}

// normalizePipelineFilters normalizes the $match stages of pipeline and of the
// pipelines its $facet, $lookup and $unionWith stages run.
func normalizePipelineFilters(pipeline *types.Array) {
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		name, spec := stage.FieldAt(0)
		switch name {
		case "$match":
			normalizeFilterField(stage, name)
		case "$facet":
			facets, ok := spec.(*types.Document)
			if !ok {
				continue
			}
			for _, branch := range facets.Values() {
				if sub, ok := branch.(*types.Array); ok {
					normalizePipelineFilters(sub)
				}
			}
		case "$lookup", "$unionWith":
			if specDoc, ok := spec.(*types.Document); ok {
				if sub, ok := uniqueField(specDoc, "pipeline").(*types.Array); ok {
					normalizePipelineFilters(sub)
				}
			}
		}
	}
}

// normalizeFilter returns filter, or a rewritten copy when it or a filter
// nested in it repeats a key: repeated top-level keys become one $and clause
// per field, as do repeated operators of a field condition.
func normalizeFilter(filter *types.Document) *types.Document {
	keys, values := filter.Keys(), filter.Values()
	var changed []any
	for i, key := range keys {
		if v, ok := normalizeCondition(key, values[i]); ok {
			if changed == nil {
				changed = slices.Clone(values)
			}
			changed[i] = v
		}
	}
	if changed != nil {
		values = changed
	}

	_, repeated := filter.FindDuplicateKey()
	var clauses []*types.Document
	for i, key := range keys {
		if split := splitRepeatedOperators(key, values[i]); split != nil {
			clauses = append(clauses, split...)
			repeated = true
			continue
		}
		clauses = append(clauses, must.NotFail(types.NewDocument(key, values[i])))
	}
	if !repeated {
		if changed == nil {
			return filter
		}
		out := types.MakeDocument(len(keys))
		for i, key := range keys {
			out.Set(key, values[i])
		}
		return out
	}
	and := types.MakeArray(len(clauses))
	for _, clause := range clauses {
		and.Append(clause)
	}
	return must.NotFail(types.NewDocument("$and", and))
}

// normalizeCondition normalizes the filters nested in one filter field (the
// clauses of a logical operator and an $elemMatch query), reporting whether
// it changed the value.
func normalizeCondition(key string, value any) (any, bool) {
	switch key {
	case "$and", "$or", "$nor":
		clauses, ok := value.(*types.Array)
		if !ok {
			return value, false
		}
		var out *types.Array
		for i := range clauses.Len() {
			clause, ok := must.NotFail(clauses.Get(i)).(*types.Document)
			if !ok {
				continue
			}
			if normalized := normalizeFilter(clause); normalized != clause {
				if out == nil {
					out = types.MakeArray(clauses.Len())
					for j := range clauses.Len() {
						out.Append(must.NotFail(clauses.Get(j)))
					}
				}
				must.NoError(out.Set(i, normalized))
			}
		}
		if out == nil {
			return value, false
		}
		return out, true
	}
	if strings.HasPrefix(key, "$") {
		return value, false
	}
	cond, ok := value.(*types.Document)
	if !ok || !isOperatorDocument(cond) {
		return value, false
	}
	query, ok := uniqueField(cond, "$elemMatch").(*types.Document)
	if !ok || isFieldOperatorExpression(query) {
		return value, false
	}
	normalized := normalizeFilter(query)
	if normalized == query {
		return value, false
	}
	out := types.MakeDocument(cond.Len())
	for i, op := range cond.Keys() {
		v := cond.Values()[i]
		if op == "$elemMatch" {
			v = normalized
		}
		out.Set(op, v)
	}
	return out, true
}

// splitRepeatedOperators returns one {key: {op: value}} clause per operator
// of a field condition that repeats an operator, or nil. A condition with
// $regex or $options stays whole, as they combine.
func splitRepeatedOperators(key string, value any) []*types.Document {
	if strings.HasPrefix(key, "$") {
		return nil
	}
	cond, ok := value.(*types.Document)
	if !ok || !isOperatorDocument(cond) {
		return nil
	}
	if _, repeated := cond.FindDuplicateKey(); !repeated {
		return nil
	}
	ops := cond.Keys()
	for _, op := range ops {
		if op == "$regex" || op == "$options" {
			return nil
		}
	}
	clauses := make([]*types.Document, len(ops))
	for i, op := range ops {
		clauses[i] = must.NotFail(types.NewDocument(key, must.NotFail(types.NewDocument(op, cond.Values()[i]))))
	}
	return clauses
}
