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
	"strings"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// A command's collation governs every string comparison in its pipeline. It
// is attached to the stage and operator documents of the command's own copy of
// the pipeline (see BindNowInPipeline), so the stages and operators built from
// them find it.

// BindCollation attaches cmp to every stage and operator document within v. A
// nil cmp attaches nothing.
func BindCollation(v any, cmp *collation.Comparator) {
	if cmp == nil {
		return
	}
	switch v := v.(type) {
	case *types.Document:
		keys := v.Keys()
		if len(keys) > 0 && strings.HasPrefix(keys[0], "$") {
			if keys[0] == "$literal" {
				return
			}
			v.Annotate(cmp)
		}
		for _, k := range keys {
			BindCollation(must.NotFail(v.Get(k)), cmp)
		}
	case *types.Array:
		for i := range v.Len() {
			BindCollation(must.NotFail(v.Get(i)), cmp)
		}
	}
}

// CollationOf returns the collation attached to doc, or nil for simple.
func CollationOf(doc *types.Document) *collation.Comparator {
	cmp, _ := doc.Annotation().(*collation.Comparator)
	return cmp
}

// collatedOperator is an operator whose result depends on comparing strings.
type collatedOperator interface {
	setCollation(cmp *collation.Comparator)
}

// collated holds the collation an operator compares strings under.
type collated struct {
	cmp *collation.Comparator
}

func (c *collated) setCollation(cmp *collation.Comparator) { c.cmp = cmp }

// compareCollated compares a and b as aggregation does, with strings
// compared under cmp when it is set.
func compareCollated(cmp *collation.Comparator, a, b any) types.CompareResult {
	if cmp == nil {
		return types.CompareForAggregation(a, b)
	}
	return types.CompareForAggregation(cmp.KeyValue(a), cmp.KeyValue(b))
}
