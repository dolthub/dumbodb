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
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
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
