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
	"sort"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// CanonicalizeFilter rewrites, in place, the document values a filter
// compares against into the sorted-key form DumboDB stores documents in, so
// that {addr: {b: 1, a: 2}} matches the document it inserted. _id values are
// left as written: DumboDB stores _id as is, and MongoDB treats _ids that
// differ only in field order as distinct.
func CanonicalizeFilter(filter *types.Document) {
	if filter == nil {
		return
	}
	for _, key := range filter.Keys() {
		value := must.NotFail(filter.Get(key))
		switch key {
		case "$and", "$or", "$nor":
			if branches, ok := value.(*types.Array); ok {
				for i := range branches.Len() {
					if branch, ok := must.NotFail(branches.Get(i)).(*types.Document); ok {
						CanonicalizeFilter(branch)
					}
				}
			}
		case "$expr":
			filter.Set(key, canonicalExpression(value))
		default:
			if strings.HasPrefix(key, "$") || key == "_id" || strings.HasPrefix(key, "_id.") {
				continue
			}
			filter.Set(key, canonicalCondition(value))
		}
	}
}

// canonicalCondition canonicalizes the condition on one field: either a
// value to equal or a document of query operators.
func canonicalCondition(value any) any {
	doc, ok := value.(*types.Document)
	if !ok || !isOperatorDocument(doc) {
		return sortedKeys(value)
	}
	for _, op := range doc.Keys() {
		operand := must.NotFail(doc.Get(op))
		switch op {
		case "$eq", "$ne", "$gt", "$gte", "$lt", "$lte", "$in", "$nin", "$all":
			doc.Set(op, sortedKeys(operand))
		case "$not":
			doc.Set(op, canonicalCondition(operand))
		case "$elemMatch":
			if sub, ok := operand.(*types.Document); ok {
				if isOperatorDocument(sub) {
					doc.Set(op, canonicalCondition(sub))
				} else {
					CanonicalizeFilter(sub)
				}
			}
		}
	}
	return doc
}

// canonicalExpression sorts the document literals in a $expr expression,
// whose value is only tested for truth.
func canonicalExpression(expr any) any {
	switch v := expr.(type) {
	case *types.Document:
		if isOperatorDocument(v) {
			for _, op := range v.Keys() {
				operand := must.NotFail(v.Get(op))
				if op == "$literal" {
					v.Set(op, sortedKeys(operand))
				} else {
					v.Set(op, canonicalExpression(operand))
				}
			}
			return v
		}
		out := sortedKeys(v).(*types.Document)
		for _, k := range out.Keys() {
			out.Set(k, canonicalExpression(must.NotFail(out.Get(k))))
		}
		return out
	case *types.Array:
		for i := range v.Len() {
			must.NoError(v.Set(i, canonicalExpression(must.NotFail(v.Get(i)))))
		}
		return v
	default:
		return v
	}
}

func isOperatorDocument(doc *types.Document) bool {
	return doc.Len() > 0 && strings.HasPrefix(doc.Keys()[0], "$")
}

// sortedKeys returns a copy of v with the keys of every document in it in
// byte order.
func sortedKeys(v any) any {
	switch x := v.(type) {
	case *types.Document:
		keys := append([]string(nil), x.Keys()...)
		sort.Strings(keys)
		out := types.MakeDocument(len(keys))
		for _, k := range keys {
			out.Set(k, sortedKeys(must.NotFail(x.Get(k))))
		}
		return out
	case *types.Array:
		out := types.MakeArray(x.Len())
		for i := range x.Len() {
			out.Append(sortedKeys(must.NotFail(x.Get(i))))
		}
		return out
	default:
		return v
	}
}
