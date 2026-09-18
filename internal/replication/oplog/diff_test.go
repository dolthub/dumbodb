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

package oplog

import (
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestApplyV2DiffNestedObjectsAndArrays(t *testing.T) {
	preImage := must.NotFail(types.NewDocument(
		"_id", int32(1),
		"removed", "old",
		"updated", "old",
		"nested", must.NotFail(types.NewDocument("keep", true, "old", int32(1))),
		"array", must.NotFail(types.NewArray(
			must.NotFail(types.NewDocument("value", int32(1))),
			"replace",
			int32(3),
		)),
	))
	diff := must.NotFail(types.NewDocument(
		"d", must.NotFail(types.NewDocument("removed", false)),
		"u", must.NotFail(types.NewDocument("updated", "new")),
		"i", must.NotFail(types.NewDocument("inserted", int32(4))),
		"snested", must.NotFail(types.NewDocument(
			"d", must.NotFail(types.NewDocument("old", false)),
			"i", must.NotFail(types.NewDocument("new", int32(2))),
		)),
		"sarray", must.NotFail(types.NewDocument(
			"a", true,
			"s0", must.NotFail(types.NewDocument("u", must.NotFail(types.NewDocument("value", int32(9))))),
			"u1", "replaced",
			"u4", int32(5),
		)),
	))
	actual, err := ApplyV2Diff(preImage, diff)
	if err != nil {
		t.Fatal(err)
	}
	expected := must.NotFail(types.NewDocument(
		"_id", int32(1),
		"updated", "new",
		"nested", must.NotFail(types.NewDocument("keep", true, "new", int32(2))),
		"array", must.NotFail(types.NewArray(
			must.NotFail(types.NewDocument("value", int32(9))),
			"replaced",
			int32(3),
			types.Null,
			int32(5),
		)),
		"inserted", int32(4),
	))
	if types.Compare(actual, expected) != types.Equal {
		t.Fatalf("post-image = %v, want %v", actual, expected)
	}
	if types.Compare(preImage, actual) == types.Equal {
		t.Fatal("ApplyV2Diff mutated or returned the pre-image")
	}
}

func TestApplyV2DiffArrayResize(t *testing.T) {
	preImage := must.NotFail(types.NewDocument("array", must.NotFail(types.NewArray(int32(1), int32(2), int32(3), int32(4)))))
	diff := must.NotFail(types.NewDocument("sarray", must.NotFail(types.NewDocument("a", true, "l", int32(2)))))
	actual, err := ApplyV2Diff(preImage, diff)
	if err != nil {
		t.Fatal(err)
	}
	expected := must.NotFail(types.NewDocument("array", must.NotFail(types.NewArray(int32(1), int32(2)))))
	if types.Compare(actual, expected) != types.Equal {
		t.Fatalf("post-image = %v, want %v", actual, expected)
	}
}

func TestApplyV2DiffTypeMismatchBecomesNull(t *testing.T) {
	preImage := must.NotFail(types.NewDocument("value", "not an object"))
	diff := must.NotFail(types.NewDocument("svalue", must.NotFail(types.NewDocument("u", must.NotFail(types.NewDocument("nested", true))))))
	actual, err := ApplyV2Diff(preImage, diff)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := actual.Get("value")
	if value != types.Null {
		t.Fatalf("mismatched sub-diff value = %v", value)
	}
}

func TestApplyV2DiffRejectsMalformedOrConflictingDiff(t *testing.T) {
	preImage := must.NotFail(types.NewDocument("value", int32(1)))
	tests := []*types.Document{
		must.NotFail(types.NewDocument("u", must.NotFail(types.NewDocument("value", int32(2))), "d", must.NotFail(types.NewDocument("value", false)))),
		must.NotFail(types.NewDocument("unknown", must.NotFail(types.NewDocument()))),
		must.NotFail(types.NewDocument("svalue", must.NotFail(types.NewDocument("a", false)))),
		must.NotFail(types.NewDocument("svalue", must.NotFail(types.NewDocument("a", true, "u01", int32(2))))),
	}
	for _, diff := range tests {
		if _, err := ApplyV2Diff(preImage, diff); err == nil {
			t.Fatalf("ApplyV2Diff accepted malformed diff %v", diff)
		}
	}
}
