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
	"errors"
	"strings"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// dolthub/dumbodb#113: specs identical to existing indexes are dropped from the
// request without disturbing the others, however many there are.
func TestValidateIndexesForCreationSkipsExisting(t *testing.T) {
	idx := func(name, field string) backends.IndexInfo {
		return backends.IndexInfo{Name: name, Key: []backends.IndexKeyPair{{Field: field}}}
	}
	existing := []backends.IndexInfo{idx("_id_", "_id"), idx("method", "methodKey"), idx("name", "fullyQualifiedName")}

	for _, tc := range []struct {
		name     string
		toCreate []backends.IndexInfo
		want     []string
	}{
		{"all exist", []backends.IndexInfo{idx("method", "methodKey"), idx("name", "fullyQualifiedName")}, nil},
		{"first exists", []backends.IndexInfo{idx("method", "methodKey"), idx("other", "o")}, []string{"other"}},
		{"middle exists", []backends.IndexInfo{idx("a", "a"), idx("name", "fullyQualifiedName"), idx("b", "b")}, []string{"a", "b"}},
		{"none exist", []backends.IndexInfo{idx("a", "a"), idx("b", "b")}, []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateIndexesForCreation("createIndexes", existing, tc.toCreate)
			if err != nil {
				t.Fatalf("validateIndexesForCreation: %v", err)
			}
			var names []string
			for _, g := range got {
				names = append(names, g.Name)
			}
			if len(names) != len(tc.want) {
				t.Fatalf("to create = %v, want %v", names, tc.want)
			}
			for i := range names {
				if names[i] != tc.want[i] {
					t.Fatalf("to create = %v, want %v", names, tc.want)
				}
			}
		})
	}
}

// dolthub/dumbodb#153: unknown index-spec options are ignored only when
// ignoreUnknownIndexOptions is set; legacy specs like { ns: ... } stay rejected.
func TestProcessIndexIgnoreUnknownIndexOptions(t *testing.T) {
	newSpec := func() *types.Document {
		return must.NotFail(types.NewDocument(
			"key", must.NotFail(types.NewDocument("a", int32(1))),
			"name", "a_1",
			"ns", "db.coll",
		))
	}

	assertErrCode := func(t *testing.T, err error, code handlererrors.ErrorCode) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error")
		}
		var ce *handlererrors.CommandError
		if !errors.As(err, &ce) || ce.Code() != code {
			t.Fatalf("expected %v, got %v", code, err)
		}
	}

	t.Run("enabled", func(t *testing.T) {
		idx, err := processIndex("createIndexes", newSpec(), true)
		if err != nil {
			t.Fatalf("processIndex: %v", err)
		}
		if idx.Name != "a_1" || len(idx.Key) != 1 || idx.Key[0].Field != "a" {
			t.Fatalf("unexpected index: %+v", idx)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		_, err := processIndex("createIndexes", newSpec(), false)
		assertErrCode(t, err, handlererrors.ErrInvalidIndexSpecificationOption)
		if !strings.Contains(err.Error(), `The field 'ns' is not valid for an index specification`) {
			t.Fatalf("unexpected error %v", err)
		}
	})

	t.Run("recognized options still work when enabled", func(t *testing.T) {
		spec := must.NotFail(types.NewDocument(
			"key", must.NotFail(types.NewDocument("a", int32(1))),
			"name", "a_1",
			"unique", true,
			"sparse", true,
			"ns", "db.coll",
		))

		idx, err := processIndex("createIndexes", spec, true)
		if err != nil {
			t.Fatalf("processIndex: %v", err)
		}
		if !idx.Unique || !idx.Sparse {
			t.Fatalf("unexpected index: %+v", idx)
		}
	})
}
