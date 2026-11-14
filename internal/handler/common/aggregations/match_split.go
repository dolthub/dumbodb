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

package aggregations

import (
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// FilterReadsField reports whether a query filter may read field. It answers
// true for any filter it cannot analyze.
func FilterReadsField(filter *types.Document, field string) bool {
	for _, key := range filter.Keys() {
		switch key {
		case "$and", "$or", "$nor":
			arr, ok := must.NotFail(filter.Get(key)).(*types.Array)
			if !ok {
				return true
			}
			for i := range arr.Len() {
				sub, ok := must.NotFail(arr.Get(i)).(*types.Document)
				if !ok || FilterReadsField(sub, field) {
					return true
				}
			}
		case "$comment":
		case "$expr":
			if exprReadsField(must.NotFail(filter.Get(key)), field) {
				return true
			}
		case "$jsonSchema":
			schema, ok := must.NotFail(filter.Get(key)).(*types.Document)
			if !ok || schemaReadsField(schema, field) {
				return true
			}
		default:
			if strings.HasPrefix(key, "$") || key == field ||
				strings.HasPrefix(key, field+".") || strings.HasPrefix(field, key+".") {
				return true
			}
		}
	}
	return false
}

// schemaReadsField reports whether a $jsonSchema may read the top-level
// field holding field: it names a path on it in required or properties, or
// constrains the fields it does not name.
func schemaReadsField(schema *types.Document, field string) bool {
	for _, key := range schema.Keys() {
		v := must.NotFail(schema.Get(key))
		switch key {
		case "bsonType", "type", "title", "description":
		case "required":
			names, ok := v.(*types.Array)
			if !ok {
				return true
			}
			for i := range names.Len() {
				if name, _ := must.NotFail(names.Get(i)).(string); sameRoot(name, field) {
					return true
				}
			}
		case "properties":
			props, ok := v.(*types.Document)
			if !ok {
				return true
			}
			for _, name := range props.Keys() {
				if sameRoot(name, field) {
					return true
				}
			}
		case "allOf", "anyOf", "oneOf":
			subs, ok := v.(*types.Array)
			if !ok {
				return true
			}
			for i := range subs.Len() {
				sub, ok := must.NotFail(subs.Get(i)).(*types.Document)
				if !ok || schemaReadsField(sub, field) {
					return true
				}
			}
		case "not":
			sub, ok := v.(*types.Document)
			if !ok || schemaReadsField(sub, field) {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// sameRoot reports whether two paths start with the same top-level field.
func sameRoot(a, b string) bool {
	rootA, _, _ := strings.Cut(a, ".")
	rootB, _, _ := strings.Cut(b, ".")
	return rootA == rootB
}

// exprReadsField reports whether an expression may read field: through a
// path on it, also as $$ROOT.<path> or $$CURRENT.<path>, or through all of
// $$ROOT or $$CURRENT.
func exprReadsField(expr any, field string) bool {
	switch v := expr.(type) {
	case string:
		if !strings.HasPrefix(v, "$") {
			return false
		}
		p := v[1:]
		if strings.HasPrefix(v, "$$") {
			name, rest, dotted := strings.Cut(v[2:], ".")
			if name != "ROOT" && name != "CURRENT" {
				return false
			}
			if !dotted {
				return true
			}
			p = rest
		}
		return p == field || strings.HasPrefix(p, field+".") || strings.HasPrefix(field, p+".")
	case *types.Array:
		for i := range v.Len() {
			if exprReadsField(must.NotFail(v.Get(i)), field) {
				return true
			}
		}
	case *types.Document:
		switch v.Command() {
		case "$literal":
			return false
		case "$getField":
			// Without input it reads $$CURRENT.
			spec, ok := must.NotFail(v.Get("$getField")).(*types.Document)
			if !ok || !spec.Has("input") {
				return true
			}
		case "$let":
			// Rebinding CURRENT changes what every path reads.
			if spec, ok := must.NotFail(v.Get("$let")).(*types.Document); ok {
				if vars, _ := spec.Get("vars"); vars != nil {
					if vars, ok := vars.(*types.Document); ok && vars.Has("CURRENT") {
						return true
					}
				}
			}
		}
		for _, k := range v.Keys() {
			if exprReadsField(must.NotFail(v.Get(k)), field) {
				return true
			}
		}
	}
	return false
}

// splitMatchAhead splits match, which follows stage, as mongod's optimizer
// does: a $lookup or $unwind lets the conjuncts of the filter that do not
// read the fields it writes run ahead of it. ahead is match itself when no
// conjunct reads them; behind is nil when none stays. It returns false when
// nothing moves.
func splitMatchAhead(stage, match *types.Document) (ahead, behind *types.Document, ok bool) {
	if stage == nil || match == nil || stage.Len() != 1 || match.Len() != 1 || match.Command() != "$match" {
		return nil, nil, false
	}
	written, ok := writtenFields(stage)
	if !ok {
		return nil, nil, false
	}
	filter, ok := must.NotFail(match.Get("$match")).(*types.Document)
	if !ok {
		return nil, nil, false
	}
	readsWritten := func(f *types.Document) bool {
		for _, w := range written {
			if FilterReadsField(f, w) {
				return true
			}
		}
		return false
	}
	if !readsWritten(filter) {
		return match, nil, true
	}

	var moved, kept []*types.Document
	for _, c := range filterConjuncts(filter) {
		switch key := c.Keys()[0]; key {
		case "$comment":
			kept = append(kept, c)
			continue
		case "$jsonSchema":
			if schema, isDoc := must.NotFail(c.Get(key)).(*types.Document); isDoc {
				parts, rest := splitSchema(schema, written)
				moved = append(moved, parts...)
				if rest != nil {
					kept = append(kept, must.NotFail(types.NewDocument(key, rest)))
				}
				continue
			}
		}
		if readsWritten(c) {
			kept = append(kept, c)
		} else {
			moved = append(moved, c)
		}
	}
	if len(moved) == 0 {
		return nil, nil, false
	}
	ahead = must.NotFail(types.NewDocument("$match", joinConjuncts(moved)))
	if len(kept) > 0 {
		behind = must.NotFail(types.NewDocument("$match", joinConjuncts(kept)))
	}
	return ahead, behind, true
}

// writtenFields returns the fields a $lookup or $unwind stage writes, or
// false for any other stage or a specification it cannot follow.
func writtenFields(stage *types.Document) ([]string, bool) {
	spec := must.NotFail(stage.Get(stage.Command()))
	switch stage.Command() {
	case "$lookup":
		d, ok := spec.(*types.Document)
		if !ok {
			return nil, false
		}
		as, _ := d.Get("as")
		field, ok := as.(string)
		return []string{field}, ok && field != "" && !strings.HasPrefix(field, "$")
	case "$unwind":
		path, index := spec, any(nil)
		if d, ok := spec.(*types.Document); ok {
			path, _ = d.Get("path")
			index, _ = d.Get("includeArrayIndex")
		}
		p, ok := path.(string)
		if !ok || len(p) < 2 || !strings.HasPrefix(p, "$") || strings.HasPrefix(p, "$$") {
			return nil, false
		}
		fields := []string{p[1:]}
		if index != nil {
			name, ok := index.(string)
			if !ok || name == "" || strings.HasPrefix(name, "$") {
				return nil, false
			}
			fields = append(fields, name)
		}
		return fields, true
	}
	return nil, false
}

// filterConjuncts returns the conditions a filter requires all of, each as a
// one-key filter, looking into $and.
func filterConjuncts(filter *types.Document) []*types.Document {
	var out []*types.Document
	for _, key := range filter.Keys() {
		v := must.NotFail(filter.Get(key))
		if key == "$and" {
			if branches, ok := v.(*types.Array); ok && branches.Len() > 0 {
				var nested []*types.Document
				for i := range branches.Len() {
					branch, ok := must.NotFail(branches.Get(i)).(*types.Document)
					if !ok || branch.Len() == 0 {
						nested = nil
						break
					}
					nested = append(nested, filterConjuncts(branch)...)
				}
				if nested != nil {
					out = append(out, nested...)
					continue
				}
			}
		}
		out = append(out, must.NotFail(types.NewDocument(key, v)))
	}
	return out
}

// joinConjuncts returns a filter requiring each of conjuncts: one document
// when their keys differ, else an $and of them.
func joinConjuncts(conjuncts []*types.Document) *types.Document {
	if len(conjuncts) == 1 {
		return conjuncts[0]
	}
	out := types.MakeDocument(len(conjuncts))
	for _, c := range conjuncts {
		key := c.Keys()[0]
		if out.Has(key) {
			branches := types.MakeArray(len(conjuncts))
			for _, c := range conjuncts {
				branches.Append(c)
			}
			return must.NotFail(types.NewDocument("$and", branches))
		}
		out.Set(key, must.NotFail(c.Get(key)))
	}
	return out
}

// splitSchema returns the parts of a $jsonSchema that do not read written,
// as mongod moves them: a required field as an $exists, and a property with
// constraints as a schema of that property alone. rest is the schema that
// must stay behind for what does not move, or nil.
func splitSchema(schema *types.Document, written []string) (parts []*types.Document, rest *types.Document) {
	readsWritten := func(name string) bool {
		for _, w := range written {
			if sameRoot(name, w) {
				return true
			}
		}
		return false
	}
	rest = types.MakeDocument(schema.Len())
	stays := false
	for _, key := range schema.Keys() {
		v := must.NotFail(schema.Get(key))
		switch key {
		case "title", "description":
		case "bsonType", "type":
			if v != "object" {
				stays = true
				rest.Set(key, v)
			}
		case "required":
			names, ok := v.(*types.Array)
			if !ok {
				stays = true
				rest.Set(key, v)
				continue
			}
			kept := types.MakeArray(0)
			for i := range names.Len() {
				name, ok := must.NotFail(names.Get(i)).(string)
				switch {
				case !ok || readsWritten(name):
					kept.Append(must.NotFail(names.Get(i)))
				case strings.Contains(name, ".") || strings.HasPrefix(name, "$"):
					parts = append(parts, must.NotFail(types.NewDocument("$jsonSchema", must.NotFail(types.NewDocument(
						"required", must.NotFail(types.NewArray(name)),
					)))))
				default:
					parts = append(parts, must.NotFail(types.NewDocument(name, must.NotFail(types.NewDocument("$exists", true)))))
				}
			}
			if kept.Len() > 0 {
				stays = true
				rest.Set(key, kept)
			}
		case "properties":
			props, ok := v.(*types.Document)
			if !ok {
				stays = true
				rest.Set(key, v)
				continue
			}
			// Every property keeps its name, which additionalProperties
			// reads, but one that moves no longer constrains its value.
			keptProps := types.MakeDocument(props.Len())
			for _, name := range props.Keys() {
				sub := must.NotFail(props.Get(name))
				switch {
				case readsWritten(name):
					stays = true
					keptProps.Set(name, sub)
				case !unconstrainedSchema(sub):
					parts = append(parts, must.NotFail(types.NewDocument("$jsonSchema", must.NotFail(types.NewDocument(
						"properties", must.NotFail(types.NewDocument(name, sub)),
					)))))
					keptProps.Set(name, types.MakeDocument(0))
				default:
					keptProps.Set(name, sub)
				}
			}
			rest.Set(key, keptProps)
		default:
			stays = true
			rest.Set(key, v)
		}
	}
	if !stays {
		return parts, nil
	}
	return parts, rest
}

// unconstrainedSchema reports whether a property schema accepts any value.
func unconstrainedSchema(schema any) bool {
	d, ok := schema.(*types.Document)
	if !ok {
		return false
	}
	for _, k := range d.Keys() {
		if k != "title" && k != "description" {
			return false
		}
	}
	return true
}
