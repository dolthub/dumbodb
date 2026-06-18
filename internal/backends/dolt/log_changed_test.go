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
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func changedSet(t *testing.T, a, b *types.Document) map[string]struct{} {
	t.Helper()
	set, err := changedFieldPaths(a, b)
	if err != nil {
		t.Fatalf("changedFieldPaths: %v", err)
	}
	return set
}

func TestChangedFieldPaths_Modified(t *testing.T) {
	a := mustDoc(t, "_id", int64(1), "status", "pending", "name", "x")
	b := mustDoc(t, "_id", int64(1), "status", "shipped", "name", "x")
	set := changedSet(t, a, b)

	if !changedSetMatches(set, "status") {
		t.Fatal("status should match (value changed)")
	}
	if changedSetMatches(set, "name") {
		t.Fatal("name should NOT match (unchanged)")
	}
	if changedSetMatches(set, "_id") {
		t.Fatal("_id is never diffed; should not match")
	}
}

func TestChangedFieldPaths_Nested(t *testing.T) {
	a := mustDoc(t, "_id", int64(1), "shipping", mustDoc(t, "carrier", "ups", "zone", int64(1)))
	b := mustDoc(t, "_id", int64(1), "shipping", mustDoc(t, "carrier", "fedex", "zone", int64(1)))
	set := changedSet(t, a, b)

	if !changedSetMatches(set, "shipping.carrier") {
		t.Fatal("shipping.carrier should match (exact)")
	}
	if !changedSetMatches(set, "shipping") {
		t.Fatal("shipping should match (ancestor of the changed nested path)")
	}
	if changedSetMatches(set, "shipping.zone") {
		t.Fatal("shipping.zone should NOT match (sibling unchanged)")
	}
}

func TestChangedFieldPaths_AddedDoc(t *testing.T) {
	// docA nil => whole document added; every field counts as changed.
	b := mustDoc(t, "_id", int64(1), "status", "new", "shipping", mustDoc(t, "carrier", "ups"))
	set := changedSet(t, nil, b)

	if !changedSetMatches(set, "status") {
		t.Fatal("status should match on an added doc")
	}
	// A wholesale add means the nested field changed too: $.shipping is an
	// ancestor of the query path $.shipping.carrier.
	if !changedSetMatches(set, "shipping.carrier") {
		t.Fatal("shipping.carrier should match on an added doc")
	}
	if changedSetMatches(set, "missing") {
		t.Fatal("absent field should not match")
	}
}

func TestChangedFieldPaths_RemovedDoc(t *testing.T) {
	a := mustDoc(t, "_id", int64(1), "status", "old")
	set := changedSet(t, a, nil)
	if !changedSetMatches(set, "status") {
		t.Fatal("status should match on a removed doc")
	}
}

func TestChangedFieldPaths_ArrayPrefix(t *testing.T) {
	tagsA := must.NotFail(types.NewArray("a", "b"))
	tagsB := must.NotFail(types.NewArray("a", "c"))
	a := mustDoc(t, "_id", int64(1), "tags", tagsA)
	b := mustDoc(t, "_id", int64(1), "tags", tagsB)
	set := changedSet(t, a, b)
	// The element change is under $.tags[...]; querying the array field matches
	// via the ancestor rule.
	if !changedSetMatches(set, "tags") {
		t.Fatalf("tags should match an element change; set=%v", set)
	}
}
