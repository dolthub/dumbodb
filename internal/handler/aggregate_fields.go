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

package handler

import (
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
)

// pipelineRootFields returns the top-level fields of the source documents that
// a pipeline reads. It returns false unless the pipeline is $match, $sort,
// $limit and $skip stages followed by $group, $count or an inclusion $project,
// which discard every field they do not read.
func pipelineRootFields(pipeline []any) ([]string, bool) {
	var roots []string
	for _, v := range pipeline {
		stage, ok := v.(*types.Document)
		if !ok || stage.Len() != 1 {
			return nil, false
		}
		name := stage.Command()
		spec, _ := stage.Get(name)
		switch name {
		case "$match":
			filter, ok := spec.(*types.Document)
			if !ok {
				return nil, false
			}
			fields, ok := common.FilterRootFields(filter)
			if !ok {
				return nil, false
			}
			for _, f := range fields {
				roots = appendRoot(roots, f)
			}
		case "$sort":
			keys, ok := spec.(*types.Document)
			if !ok {
				return nil, false
			}
			names := keys.Keys()
			for i, v := range keys.Values() {
				if _, isDoc := v.(*types.Document); isDoc {
					return nil, false
				}
				roots = appendRoot(roots, names[i])
			}
		case "$limit", "$skip":
		case "$count":
			return roots, true
		case "$group":
			if !appendExprRootFields(spec, &roots) {
				return nil, false
			}
			return roots, true
		case "$project":
			projection, ok := spec.(*types.Document)
			if !ok {
				return nil, false
			}
			roots = appendRoot(roots, "_id")
			if included, ok := appendProjectStageRootFields(projection, &roots, true); !ok || !included {
				return nil, false
			}
			return roots, true
		default:
			return nil, false
		}
	}
	return nil, false
}

// appendProjectStageRootFields adds the fields an inclusion $project reads and
// reports whether it includes anything. It returns false for exclusions.
func appendProjectStageRootFields(projection *types.Document, roots *[]string, top bool) (included, ok bool) {
	keys := projection.Keys()
	for i, v := range projection.Values() {
		key := keys[i]
		switch v := v.(type) {
		case bool, int32, int64, float64:
			if !isTruthy(v) {
				if top && key == "_id" {
					continue
				}
				return false, false
			}
			*roots = appendRoot(*roots, key)
		case *types.Document:
			if op := v.Command(); strings.HasPrefix(op, "$") {
				if op == "$meta" || !appendExprRootFields(v, roots) {
					return false, false
				}
			} else {
				*roots = appendRoot(*roots, key)
				if _, ok := appendProjectStageRootFields(v, roots, false); !ok {
					return false, false
				}
			}
		case string, *types.Array:
			if !appendExprRootFields(v, roots) {
				return false, false
			}
		case types.Binary, types.ObjectID, time.Time, types.NullType, types.Regex, types.Timestamp:
		default:
			return false, false
		}
		included = true
	}
	return included, true
}

// appendExprRootFields adds the fields an aggregation expression reads. It
// returns false for expressions that can read the whole document.
func appendExprRootFields(expr any, roots *[]string) bool {
	switch e := expr.(type) {
	case string:
		if rest, ok := strings.CutPrefix(e, "$$"); ok {
			name, _, _ := strings.Cut(rest, ".")
			return name != "ROOT" && name != "CURRENT"
		}
		if rest, ok := strings.CutPrefix(e, "$"); ok {
			*roots = appendRoot(*roots, rest)
		}
		return true
	case *types.Array:
		for i := 0; i < e.Len(); i++ {
			v, _ := e.Get(i)
			if !appendExprRootFields(v, roots) {
				return false
			}
		}
		return true
	case *types.Document:
		keys, values := e.Keys(), e.Values()
		for i, k := range keys {
			switch k {
			case "$function", "$accumulator", "$getField", "$setField", "$unsetField":
				return false
			case "$literal":
				continue
			case "sortBy":
				sortBy, ok := values[i].(*types.Document)
				if !ok {
					return false
				}
				for _, f := range sortBy.Keys() {
					*roots = appendRoot(*roots, f)
				}
				continue
			}
			if !appendExprRootFields(values[i], roots) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

func appendRoot(roots []string, path string) []string {
	root, _, _ := strings.Cut(path, ".")
	if slices.Contains(roots, root) {
		return roots
	}
	return append(roots, root)
}
