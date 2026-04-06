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
	"context"
	"testing"

	"github.com/dolthub/docudolt/internal/backends"
)

// TestDocuDoltCurrentBranch_ReturnsBranchName verifies that the dolt backend's
// DocuDoltCurrentBranch echoes the branch name from CurrentBranchParams.
//
// The handler layer enforces read-only restrictions before calling the backend,
// so the backend always receives a clean branch name and simply returns it.
func TestDocuDoltCurrentBranch_ReturnsBranchName(t *testing.T) {
	t.Parallel()

	b := newTestBackend(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		branch string
	}{
		{"main", "main"},
		{"feature_branch", "feature-x"},
		// Tags are syntactically indistinguishable from branch names at the handler
		// parse layer; the backend sees the tag name and returns it as-is.
		{"tag_like_v1_0", "v1.0"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res, err := b.DocuDoltCurrentBranch(ctx, &backends.CurrentBranchParams{
				DBName: "testdb",
				Branch: tc.branch,
			})
			if err != nil {
				t.Fatalf("DocuDoltCurrentBranch(%q): unexpected error: %v", tc.branch, err)
			}
			if res.Branch != tc.branch {
				t.Errorf("DocuDoltCurrentBranch(%q): got %q, want %q", tc.branch, res.Branch, tc.branch)
			}
		})
	}
}
