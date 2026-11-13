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
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// bubbleMatchesBeforeLookups is the swap-until-stable definition the linear
// moveMatchesBeforeLookups must agree with.
func bubbleMatchesBeforeLookups(pipeline []any) []any {
	reads := func(filter *types.Document, path string) bool {
		var walk func(*types.Document) bool
		walk = func(f *types.Document) bool {
			for _, k := range f.Keys() {
				switch {
				case k == "$and" || k == "$or" || k == "$nor":
					arr := must.NotFail(f.Get(k)).(*types.Array)
					for i := 0; i < arr.Len(); i++ {
						if walk(must.NotFail(arr.Get(i)).(*types.Document)) {
							return true
						}
					}
				case strings.HasPrefix(k, "$"):
					return true
				case k == path || strings.HasPrefix(k, path+".") || strings.HasPrefix(path, k+"."):
					return true
				}
			}
			return false
		}
		return walk(filter)
	}
	out := append([]any{}, pipeline...)
	for moved := true; moved; {
		moved = false
		for i := 0; i+1 < len(out); i++ {
			l, m := out[i].(*types.Document), out[i+1].(*types.Document)
			if l.Command() != "$lookup" || m.Command() != "$match" {
				continue
			}
			as, _ := specString(must.NotFail(l.Get("$lookup")).(*types.Document), "as")
			if reads(must.NotFail(m.Get("$match")).(*types.Document), as) {
				continue
			}
			out[i], out[i+1] = out[i+1], out[i]
			moved = true
		}
	}
	return out
}

func TestMoveMatchesBeforeLookupsMatchesBubbleOrder(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	fields := []string{"a", "j", "j.x", "k", "k.y", "z"}
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 2000; n++ {
		var pipeline []any
		for i := 0; i < 2+r.Intn(10); i++ {
			switch r.Intn(4) {
			case 0, 1:
				pipeline = append(pipeline, doc("$lookup", doc("from", "f", "localField", "a", "foreignField", "b", "as", []string{"j", "k", "j.x"}[r.Intn(3)])))
			case 2:
				var filter *types.Document
				switch r.Intn(4) {
				case 0:
					filter = doc("$or", must.NotFail(types.NewArray(doc(fields[r.Intn(len(fields))], int32(1)), doc(fields[r.Intn(len(fields))], int32(2)))))
				case 1:
					filter = doc("$expr", doc("$eq", must.NotFail(types.NewArray("$a", int32(1)))))
				default:
					filter = doc(fields[r.Intn(len(fields))], int32(1))
				}
				pipeline = append(pipeline, doc("$match", filter))
			default:
				pipeline = append(pipeline, doc("$unwind", "$j"))
			}
		}
		got, want := moveMatchesBeforeLookups(pipeline), bubbleMatchesBeforeLookups(pipeline)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("pipeline %d: stage %d is %s, want %s", n, i, types.FormatAnyValue(got[i]), types.FormatAnyValue(want[i]))
			}
		}
	}
}

func TestMoveMatchesBeforeLookupsIsLinear(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	var pipeline []any
	for i := 0; i < 50_000; i++ {
		pipeline = append(pipeline, doc("$lookup", doc("from", "x", "localField", "a", "foreignField", "b", "as", "j")))
	}
	for i := 0; i < 50_000; i++ {
		pipeline = append(pipeline, doc("$match", doc("z", int32(1))))
	}
	start := time.Now()
	moveMatchesBeforeLookups(pipeline)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("100,000 stages took %v", elapsed)
	}
}
