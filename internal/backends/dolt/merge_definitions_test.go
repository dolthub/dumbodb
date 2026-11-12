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

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// A definition, a validator or a view, merges automatically only when both sides' full definitions are identical:
// whether two different definitions combine correctly depends on the application. Each test merges feat into main:
//
//	c0 -- m1    main
//	  \-- f1    feat

// mergeDefinitionBranches builds c0, m1 and f1, merges feat into main, and returns the conflicts, nil when the merge is
// clean.
func mergeDefinitionBranches(t *testing.T, b *Backend, mode string, withView bool, main, feat edit) *backends.ConflictsResult {
	t.Helper()
	createItems(t, b, mode)
	insertDoc(t, b, "testdb", "items", seedItem(t, 1))
	setValidator(minU(0))(t, b, "main")
	if withView {
		addView("c0")(t, b, "main")
	}
	commitDB(t, b, "testdb", "c0")
	branchFrom(t, b, "testdb", "main", "feat")
	if main != nil {
		main(t, b, "main")
		commitBranch(t, b, "testdb", "main", "m1")
	}
	if feat != nil {
		feat(t, b, "feat")
		commitBranch(t, b, "testdb", "feat", "f1")
	}
	return mergeTips(t, b, "main", "feat")
}

func validatorWith(fields ...string) func(t *testing.T) *types.Document {
	return func(t *testing.T) *types.Document {
		doc := minU(0)(t)
		for _, field := range fields {
			doc.Set(field, mustDoc(t, "$exists", true))
		}
		return doc
	}
}

func TestMergeValidatorsOnlyWhenIdentical(t *testing.T) {
	tests := []struct {
		name       string
		main, feat edit
		conflict   bool
		want       func(t *testing.T) *types.Document
	}{
		{"different fields added", setValidator(validatorWith("a")), setValidator(validatorWith("b")), true, nil},
		{"one field set to different values", setValidator(minU(1)), setValidator(minU(2)), true, nil},
		{"identical changes", setValidator(minU(5)), setValidator(minU(5)), false, minU(5)},
		{"one side changes it", nil, setValidator(minU(7)), false, minU(7)},
	}
	for _, tt := range tests {
		for _, mode := range mergeModes {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				b := newTestBackend(t)
				res := mergeDefinitionBranches(t, b, mode, false, tt.main, tt.feat)
				if tt.conflict {
					require.NotNil(t, res, "the merge must conflict")
					ours := virtualCatalog(t, b, branchHash(t, b, "main"), "items").Validator
					finishWithOurs(t, b, "main")
					meta := virtualCatalog(t, b, branchHash(t, b, "main"), "items")
					require.True(t, documentsEqual(ours, meta.Validator), "want ours %v, got %v", ours, meta.Validator)
					return
				}
				require.Nil(t, res, "the merge must be clean")
				meta := virtualCatalog(t, b, branchHash(t, b, "main"), "items")
				require.NotNil(t, meta)
				require.True(t, documentsEqual(tt.want(t), meta.Validator), "want %v, got %v", tt.want(t), meta.Validator)
			})
		}
	}
}

func TestMergeViewsOnlyWhenIdentical(t *testing.T) {
	tests := []struct {
		name       string
		main, feat edit
		conflict   bool
		want       string
	}{
		{"identical changes", setView("same"), setView("same"), false, "same"},
		{"different changes", setView("m"), setView("f"), true, ""},
		{"one side changes it", nil, setView("f"), false, "f"},
	}
	for _, tt := range tests {
		for _, mode := range mergeModes {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				b := newTestBackend(t)
				res := mergeDefinitionBranches(t, b, mode, true, tt.main, tt.feat)
				if tt.conflict {
					require.NotNil(t, res, "the merge must conflict")
					ours := virtualView(t, b, branchHash(t, b, "main"), "v")
					finishWithOurs(t, b, "main")
					requireSameView(t, ours, virtualView(t, b, branchHash(t, b, "main"), "v"))
					return
				}
				require.Nil(t, res, "the merge must be clean")
				vm := virtualView(t, b, branchHash(t, b, "main"), "v")
				require.NotNil(t, vm)
				require.True(t, reflectEqual(matchPipeline(t, tt.want), vm.Pipeline), "want pipeline %q, got %v", tt.want, vm.Pipeline)
			})
		}
	}
}
