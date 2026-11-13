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
	"time"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// $$NOW is one value for a whole command in mongod. The Bind functions
// return copies of a command's pipelines, filters and projections with each
// expression reference to $$NOW replaced by that value; a filter's values
// outside $expr are left alone, as there "$$NOW" is a literal string.

// BindNowInPipeline binds $$NOW in each stage of pipeline.
func BindNowInPipeline(pipeline *types.Array, now time.Time) *types.Array {
	if pipeline == nil {
		return nil
	}
	out := types.MakeArray(pipeline.Len())
	for i := range pipeline.Len() {
		v := must.NotFail(pipeline.Get(i))
		if stage, ok := v.(*types.Document); ok {
			v = BindNowInStage(stage, now)
		}
		out.Append(v)
	}
	return out
}

// BindNowInStage binds $$NOW in one aggregation stage.
func BindNowInStage(stage *types.Document, now time.Time) *types.Document {
	if stage.Len() != 1 {
		return stage
	}
	name := stage.Command()
	spec := must.NotFail(stage.Get(name))
	switch name {
	case "$match":
		if filter, ok := spec.(*types.Document); ok {
			spec = BindNowInFilter(filter, now)
		}
	case "$lookup", "$unionWith", "$graphLookup", "$geoNear", "$merge":
		if d, ok := spec.(*types.Document); ok {
			spec = bindNowInStageSpec(d, now)
		}
	case "$facet":
		if d, ok := spec.(*types.Document); ok {
			facets := types.MakeDocument(d.Len())
			for _, k := range d.Keys() {
				v := must.NotFail(d.Get(k))
				if p, ok := v.(*types.Array); ok {
					v = BindNowInPipeline(p, now)
				}
				facets.Set(k, v)
			}
			spec = facets
		}
	default:
		spec = bindNowInExpression(spec, now)
	}
	return must.NotFail(types.NewDocument(name, spec))
}

// bindNowInStageSpec binds $$NOW in the parts of a stage specification that
// hold pipelines, filters or expressions.
func bindNowInStageSpec(spec *types.Document, now time.Time) *types.Document {
	out := types.MakeDocument(spec.Len())
	for _, k := range spec.Keys() {
		v := must.NotFail(spec.Get(k))
		switch k {
		case "pipeline", "whenMatched":
			if p, ok := v.(*types.Array); ok {
				v = BindNowInPipeline(p, now)
			}
		case "let", "startWith", "near":
			v = bindNowInExpression(v, now)
		case "query", "restrictSearchWithMatch":
			if f, ok := v.(*types.Document); ok {
				v = BindNowInFilter(f, now)
			}
		}
		out.Set(k, v)
	}
	return out
}

// BindNowInFilter binds $$NOW in the $expr clauses of a query filter.
func BindNowInFilter(filter *types.Document, now time.Time) *types.Document {
	if filter == nil {
		return nil
	}
	out := types.MakeDocument(filter.Len())
	for _, k := range filter.Keys() {
		v := must.NotFail(filter.Get(k))
		switch k {
		case "$expr":
			v = bindNowInExpression(v, now)
		case "$and", "$or", "$nor":
			if clauses, ok := v.(*types.Array); ok {
				bound := types.MakeArray(clauses.Len())
				for i := range clauses.Len() {
					c := must.NotFail(clauses.Get(i))
					if d, ok := c.(*types.Document); ok {
						c = BindNowInFilter(d, now)
					}
					bound.Append(c)
				}
				v = bound
			}
		}
		out.Set(k, v)
	}
	return out
}

// BindNowInProjection binds $$NOW in a find projection's expressions.
func BindNowInProjection(projection *types.Document, now time.Time) *types.Document {
	if projection == nil {
		return nil
	}
	return bindNowInExpression(projection, now).(*types.Document)
}

func bindNowInExpression(v any, now time.Time) any {
	switch v := v.(type) {
	case string:
		if v == "$$NOW" {
			lit := must.NotFail(types.NewDocument("$literal", now))
			lit.Annotate(boundNow{})
			return lit
		}
	case *types.Array:
		out := types.MakeArray(v.Len())
		for i := range v.Len() {
			out.Append(bindNowInExpression(must.NotFail(v.Get(i)), now))
		}
		return out
	case *types.Document:
		if v.Len() == 1 && v.Command() == "$literal" {
			return v
		}
		out := types.MakeDocument(v.Len())
		for _, k := range v.Keys() {
			out.Set(k, bindNowInExpression(must.NotFail(v.Get(k)), now))
		}
		return out
	}
	return v
}

// boundNow marks the literal that stands for $$NOW, which mongod does not
// treat as a constant expression.
type boundNow struct{}

// isBoundNow reports whether doc is the literal bound for $$NOW.
func isBoundNow(doc *types.Document) bool {
	_, ok := doc.Annotation().(boundNow)
	return ok
}

// CommandNow is the value of $$NOW for a command starting now, at the
// millisecond precision of a BSON date.
func CommandNow() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}
