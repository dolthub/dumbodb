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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
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
		{"full commit hash", "na7kfra98h45fr2u5qtr30o2ggm7vh61"},
		{"abbreviated hash", "a1b2c3d"},
		{"relative ancestor tilde-1", "main~1"},
		{"relative ancestor tilde-3", "main~3"},
		{"relative ancestor feature branch", "feature-x~2"},
		{"relative ancestor count zero", "main~0"},
		{"HEAD alias", "HEAD"},
		{"HEAD tilde-1", "HEAD~1"},
		{"HEAD tilde-0", "HEAD~0"},
		{"HEAD tilde-5", "HEAD~5"},
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
		{"HEAD caret", "HEAD^"},
		{"HEAD caret N", "HEAD^2"},
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
		name         string
		encoded      string
		wantDBName   string
		wantRootish  string
		wantReadOnly bool
	}{
		{"no separator defaults to main writable", "mydb", "mydb", "main", false},
		{"branch separator main", "mydb__d_main", "mydb", "main", false},
		{"feature branch writable", "mydb__d_feature-x", "mydb", "feature-x", false},
		{"tag name not all-base32 writable", "mydb__d_v1.0", "mydb", "v1.0", false},
		{"commit hash read-only", "mydb__d_na7kfra98h45fr2u5qtr30o2ggm7vh61", "mydb", "na7kfra98h45fr2u5qtr30o2ggm7vh61", true},
		{"relative ancestor read-only", "mydb__d_main~3", "mydb", "main~3", true},
		{"HEAD alias rewrites to main", "mydb__d_HEAD", "mydb", "main", false},
		{"HEAD tilde-1 rewrites to main tilde-1", "mydb__d_HEAD~1", "mydb", "main~1", true},
		{"HEAD tilde-0 rewrites to main tilde-0", "mydb__d_HEAD~0", "mydb", "main~0", true},
		{"db name with underscore", "my_db__d_main", "my_db", "main", false},
		{"double underscore db name treated as plain", "__", "__", "main", false},
		{"leading double underscore treated as plain", "__main", "__main", "main", false},
		// All-digit suffix after __d_ (e.g. UnixNano timestamp): whole name treated as plain DB.
		{"all-digit suffix treated as plain DB", "parity_sometest__d_1775505756999075683", "parity_sometest__d_1775505756999075683", "main", false},
		{"short all-digit suffix treated as plain DB", "mydb__d_12345", "mydb__d_12345", "main", false},
	}

	for _, tc := range validCases {
		tc := tc
		t.Run("valid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			dbName, rootish, readOnly, err := branchFromDBName(tc.encoded)
			if err != nil {
				t.Fatalf("branchFromDBName(%q) unexpected error: %v", tc.encoded, err)
			}
			if dbName != tc.wantDBName {
				t.Errorf("branchFromDBName(%q) dbName = %q, want %q", tc.encoded, dbName, tc.wantDBName)
			}
			if rootish != tc.wantRootish {
				t.Errorf("branchFromDBName(%q) rootish = %q, want %q", tc.encoded, rootish, tc.wantRootish)
			}
			if readOnly != tc.wantReadOnly {
				t.Errorf("branchFromDBName(%q) readOnly = %v, want %v", tc.encoded, readOnly, tc.wantReadOnly)
			}
		})
	}

	invalidCases := []struct {
		name    string
		encoded string
	}{
		{"HEAD caret", "mydb__d_HEAD^"},
		{"reflog syntax", "mydb__d_main@{yesterday}"},
		{"range syntax", "mydb__d_main..feature"},
		{"regex search", "mydb__d_:/fix bug"},
		{"caret deref", "mydb__d_v1.0^{commit}"},
		{"empty rootish", "mydb__d_"},
	}

	for _, tc := range invalidCases {
		tc := tc
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := branchFromDBName(tc.encoded)
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

func TestRootishIsReadOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rootish  string
		readOnly bool
	}{
		// Branch names (writable)
		{"main", false},
		{"feature", false},
		{"feature-branch", false},
		{"release/v2", false},

		// Dolt commit hashes (read-only): exactly 32 lowercase base32 chars (0-9a-v).
		// Abbreviated hashes are not detectable at parse time; they resolve as branches.
		{"na7kfra98h45fr2u5qtr30o2ggm7vh61", true}, // full 32-char Dolt hash
		{"00000000000000000000000000000000", true},  // all-zero hash (edge case)

		// Abbreviated hash-like strings — treated as branches at parse time (writable).
		{"abc123", false},
		{"deadbeef", false},
		{"a1b2c3d4e5f6", false},

		// Ancestor expressions (read-only): contain ~
		{"main~1", true},
		{"main~3", true},
		{"feature~10", true},

		// Tag-like names that look like branches (writable — tags indistinguishable from branches at parse time)
		{"v1.0", false},
		{"v2.3.4", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.rootish, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.readOnly, rootishIsReadOnly(tt.rootish))
		})
	}
}

func TestEnforceWritableRootish(t *testing.T) {
	t.Parallel()

	tests := []struct {
		encodedDB string
		wantErr   bool
		errMsg    string
	}{
		{"mydb", false, ""},
		{"mydb__d_main", false, ""},
		{"mydb__d_feature", false, ""},
		{"mydb__d_HEAD", false, ""},
		{"mydb__d_na7kfra98h45fr2u5qtr30o2ggm7vh61", true, "cannot write to a read-only database snapshot"},
		{"mydb__d_main~1", true, "cannot write to a read-only database snapshot"},
		{"mydb__d_HEAD~1", true, "cannot write to a read-only database snapshot"},
		{"mydb__d_HEAD~0", true, "cannot write to a read-only database snapshot"},
		{"mydb__d_00000000000000000000000000000000", true, "cannot write to a read-only database snapshot"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.encodedDB, func(t *testing.T) {
			t.Parallel()

			err := enforceWritableRootish(tt.encodedDB)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
