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
	"testing"

	"github.com/dolthub/dongo/internal/handler/handlererrors"
)

func TestParseRootish(t *testing.T) {
	t.Parallel()

	validCases := []struct {
		name    string
		rootish string
	}{
		{"bare branch name", "main"},
		{"hyphenated branch name", "feature-x"},
		{"tag name", "v1.0"},
		{"release tag", "release-2024"},
		{"full commit hash", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"},
		{"abbreviated hash", "a1b2c3d"},
		{"relative ancestor tilde-1", "main~1"},
		{"relative ancestor tilde-3", "main~3"},
		{"relative ancestor feature branch", "feature-x~2"},
		{"relative ancestor count zero", "main~0"},
	}

	for _, tc := range validCases {
		tc := tc
		t.Run("valid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if err := parseRootish(tc.rootish); err != nil {
				t.Errorf("parseRootish(%q) returned unexpected error: %v", tc.rootish, err)
			}
		})
	}

	invalidCases := []struct {
		name    string
		rootish string
	}{
		{"HEAD", "HEAD"},
		{"HEAD tilde-1", "HEAD~1"},
		{"HEAD tilde-3", "HEAD~3"},
		{"HEAD caret", "HEAD^"},
		{"reflog yesterday", "main@{yesterday}"},
		{"reflog 5 minutes ago", "@{5 minutes ago}"},
		{"range double-dot", "main..feature"},
		{"range triple-dot", "main...feature"},
		{"regex commit search", ":/fix bug"},
		{"type deref commit", "v1.0^{commit}"},
		{"type deref empty", "v1.0^{}"},
		{"caret parent N", "main^2"},
		{"bare caret", "main^"},
		{"empty string", ""},
	}

	for _, tc := range invalidCases {
		tc := tc
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			err := parseRootish(tc.rootish)
			if err == nil {
				t.Errorf("parseRootish(%q) expected error, got nil", tc.rootish)
				return
			}

			var cmdErr *handlererrors.CommandError
			if !errors.As(err, &cmdErr) {
				t.Errorf("parseRootish(%q) expected *CommandError, got %T: %v", tc.rootish, err, err)
				return
			}

			if cmdErr.Code() != handlererrors.ErrOperationFailed {
				t.Errorf("parseRootish(%q) expected code ErrOperationFailed (%d), got %d",
					tc.rootish, handlererrors.ErrOperationFailed, cmdErr.Code())
			}

			if cmdErr.Error() == "" {
				t.Errorf("parseRootish(%q) error message must not be empty", tc.rootish)
			}
		})
	}
}

func TestBranchFromDBName(t *testing.T) {
	t.Parallel()

	validCases := []struct {
		name           string
		encoded        string
		wantDBName     string
		wantRootish    string
	}{
		{"no separator defaults to main", "mydb", "mydb", "main"},
		{"branch separator", "mydb__main", "mydb", "main"},
		{"feature branch", "mydb__feature-x", "mydb", "feature-x"},
		{"tag name", "mydb__v1.0", "mydb", "v1.0"},
		{"commit hash", "mydb__a1b2c3d", "mydb", "a1b2c3d"},
		{"relative ancestor", "mydb__main~3", "mydb", "main~3"},
		{"db name with underscore", "my_db__main", "my_db", "main"},
	}

	for _, tc := range validCases {
		tc := tc
		t.Run("valid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			dbName, rootish, err := branchFromDBName(tc.encoded)
			if err != nil {
				t.Fatalf("branchFromDBName(%q) unexpected error: %v", tc.encoded, err)
			}
			if dbName != tc.wantDBName {
				t.Errorf("branchFromDBName(%q) dbName = %q, want %q", tc.encoded, dbName, tc.wantDBName)
			}
			if rootish != tc.wantRootish {
				t.Errorf("branchFromDBName(%q) rootish = %q, want %q", tc.encoded, rootish, tc.wantRootish)
			}
		})
	}

	invalidCases := []struct {
		name    string
		encoded string
	}{
		{"HEAD in rootish", "mydb__HEAD"},
		{"HEAD-relative", "mydb__HEAD~1"},
		{"reflog syntax", "mydb__main@{yesterday}"},
		{"range syntax", "mydb__main..feature"},
		{"regex search", "mydb__:/fix bug"},
		{"caret deref", "mydb__v1.0^{commit}"},
		{"empty rootish", "mydb__"},
	}

	for _, tc := range invalidCases {
		tc := tc
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := branchFromDBName(tc.encoded)
			if err == nil {
				t.Errorf("branchFromDBName(%q) expected error, got nil", tc.encoded)
				return
			}

			var cmdErr *handlererrors.CommandError
			if !errors.As(err, &cmdErr) {
				t.Errorf("branchFromDBName(%q) expected *CommandError, got %T: %v", tc.encoded, err, err)
				return
			}

			if cmdErr.Code() != handlererrors.ErrOperationFailed {
				t.Errorf("branchFromDBName(%q) expected ErrOperationFailed, got code %d", tc.encoded, cmdErr.Code())
			}

			// Error message must name the rejected form so it's actionable.
			msg := cmdErr.Error()
			if msg == "" {
				t.Errorf("branchFromDBName(%q) error message must not be empty", tc.encoded)
			}
		})
	}
}
