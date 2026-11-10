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

package aggregations

import (
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// GetPushdownQuery gets pushdown query ($match and $sort) for aggregation.
//
// If the first two stages are either $match, $sort, or a combination of them, we can push them down.
// In this case, we return the first match and sort statements to pushdown.
// If $match stage is not present, match is returned as nil.
// If $sort stage is not present, sort is returned as nil.
func GetPushdownQuery(stagesDocs []any) (match *types.Document, sort *types.Document) {
	if len(stagesDocs) == 0 {
		return
	}

	stagesToPushdown := []any{stagesDocs[0]}

	if len(stagesDocs) > 1 {
		stagesToPushdown = append(stagesToPushdown, stagesDocs[1])
	}

	for _, s := range stagesToPushdown {
		stage, isDoc := s.(*types.Document)

		if !isDoc {
			return nil, nil
		}

		switch {
		case stage.Has("$match"):
			matchQuery := must.NotFail(stage.Get("$match"))
			query, isDoc := matchQuery.(*types.Document)

			if !isDoc || match != nil {
				continue
			}

			match = query

		case stage.Has("$sort"):
			sortQuery := must.NotFail(stage.Get("$sort"))
			query, isDoc := sortQuery.(*types.Document)

			if !isDoc || sort != nil {
				continue
			}

			sort = query

		default:
			// not $match nor $sort, we shouldn't continue pushdown
			return
		}
	}

	return
}

// MoveMatchesAhead moves each $match ahead of a $project, $set or $addFields
// before it when every field the match reads passes through that stage
// unchanged or renamed from a plain path ({z: "$a"}), rewriting renamed fields
// to their source, as mongod's optimizer does. The result is equivalent, and
// the moved match can be pushed down to an index.
//
// One pass: after a match moves, it is compared with the stage now before it,
// so each match moves as far as it can. The cost is bounded by the pipeline
// length limit (MaxPipelineLength) squared.
func MoveMatchesAhead(stagesDocs []any) []any {
	out := append([]any{}, stagesDocs...)
	for i := 0; i+1 < len(out); {
		shape, _ := out[i].(*types.Document)
		match, _ := out[i+1].(*types.Document)
		if moved, ok := moveMatchAhead(shape, match); ok {
			out[i], out[i+1] = moved, shape
			if i > 0 {
				i--
			}
			continue
		}
		i++
	}
	return out
}

// moveMatchAhead returns match rewritten to run before shape, or false when it
// cannot move ahead of it.
func moveMatchAhead(shape, match *types.Document) (*types.Document, bool) {
	if shape == nil || match == nil || shape.Len() != 1 || match.Len() != 1 || match.Command() != "$match" {
		return nil, false
	}
	fields, ok := stageFieldSources(shape)
	if !ok {
		return nil, false
	}
	filter, ok := must.NotFail(match.Get("$match")).(*types.Document)
	if !ok {
		return nil, false
	}
	rewritten, ok := rewriteFilterFields(filter, fields)
	if !ok {
		return nil, false
	}
	return must.NotFail(types.NewDocument("$match", rewritten)), true
}

// fieldSources says where a stage's output fields come from: source returns
// the input path of an output top-level field, or false when the stage
// computes or drops it.
type fieldSources func(field string) (string, bool)

// stageFieldSources returns the field sources of a $project, $set or
// $addFields stage, or false for any other stage or a shape it cannot follow.
func stageFieldSources(stage *types.Document) (fieldSources, bool) {
	spec, ok := must.NotFail(stage.Get(stage.Command())).(*types.Document)
	if !ok {
		return nil, false
	}
	renames := map[string]string{}
	opaque := map[string]bool{}
	switch stage.Command() {
	case "$set", "$addFields":
		for _, k := range spec.Keys() {
			top := strings.SplitN(k, ".", 2)[0]
			if src, ok := plainFieldPath(must.NotFail(spec.Get(k))); ok && k == top {
				renames[k] = src
			} else {
				opaque[top] = true
			}
		}
		return func(field string) (string, bool) {
			if src, ok := renames[field]; ok {
				return src, true
			}
			return field, !opaque[field]
		}, true
	case "$project":
		included := map[string]bool{"_id": true}
		exclusion := false
		for _, k := range spec.Keys() {
			v := must.NotFail(spec.Get(k))
			top := strings.SplitN(k, ".", 2)[0]
			switch {
			case isProjectionFlag(v, false):
				if k != "_id" {
					exclusion = true
				}
				opaque[top] = true
				delete(included, top)
			case isProjectionFlag(v, true) && k == top:
				included[k] = true
			default:
				if src, ok := plainFieldPath(v); ok && k == top {
					renames[k] = src
				} else {
					opaque[top] = true
				}
			}
		}
		return func(field string) (string, bool) {
			if src, ok := renames[field]; ok {
				return src, true
			}
			if opaque[field] {
				return "", false
			}
			return field, exclusion || included[field]
		}, true
	}
	return nil, false
}

// plainFieldPath returns the path of a "$a.b" field reference.
func plainFieldPath(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "$") || strings.HasPrefix(s, "$$") || len(s) < 2 {
		return "", false
	}
	return s[1:], true
}

// isProjectionFlag reports whether v includes (want true) or excludes a field.
func isProjectionFlag(v any, want bool) bool {
	switch v := v.(type) {
	case bool:
		return v == want
	case int32:
		return (v != 0) == want
	case int64:
		return (v != 0) == want
	case float64:
		return (v != 0) == want
	}
	return false
}

// rewriteFilterFields rewrites filter's field paths to the input paths
// sources gives, or returns false when it reads a field the stage computes or
// drops, or holds an operator other than $and, $or and $nor.
func rewriteFilterFields(filter *types.Document, sources fieldSources) (*types.Document, bool) {
	out := types.MakeDocument(filter.Len())
	for _, k := range filter.Keys() {
		v := must.NotFail(filter.Get(k))
		switch {
		case k == "$and" || k == "$or" || k == "$nor":
			arr, ok := v.(*types.Array)
			if !ok {
				return nil, false
			}
			branches := types.MakeArray(arr.Len())
			for i := 0; i < arr.Len(); i++ {
				sub, ok := must.NotFail(arr.Get(i)).(*types.Document)
				if !ok {
					return nil, false
				}
				rewritten, ok := rewriteFilterFields(sub, sources)
				if !ok {
					return nil, false
				}
				branches.Append(rewritten)
			}
			out.Set(k, branches)
		case strings.HasPrefix(k, "$"):
			return nil, false
		default:
			top, rest, dotted := strings.Cut(k, ".")
			src, ok := sources(top)
			if !ok {
				return nil, false
			}
			if dotted {
				src += "." + rest
			}
			if out.Has(src) {
				return nil, false
			}
			out.Set(src, v)
		}
	}
	return out, true
}
