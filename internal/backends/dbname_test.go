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

package backends

import (
	"strings"
	"testing"
)

func TestSplitEncodedDBName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		encoded     string
		wantDBName  string
		wantRootish string
	}{
		{"plain name", "mydb", "mydb", ""},
		{"branch", "mydb@main", "mydb", "main"},
		{"commit hash", "mydb@" + strings.Repeat("q", 32), "mydb", strings.Repeat("q", 32)},
		{"ancestor expression", "mydb@main~2", "mydb", "main~2"},
		{"percent-decoded slash", "mydb@feature%2Ffoo", "mydb", "feature/foo"},
		{"percent-decoded dot", "mydb@v1%2E0", "mydb", "v1.0"},
		{"bad encoding falls back to raw", "mydb@bad%zz", "mydb", "bad%zz"},
		{"all-digit suffix stays plain", "prefix@1775505756999075683", "prefix@1775505756999075683", ""},
		{"all-digit 32 chars is a hash", "mydb@" + strings.Repeat("1", 32), "mydb", strings.Repeat("1", 32)},
		{"leading separator is not a split", "@main", "@main", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dbName, rootish := SplitEncodedDBName(tc.encoded)
			if dbName != tc.wantDBName {
				t.Errorf("dbName = %q, want %q", dbName, tc.wantDBName)
			}
			if rootish != tc.wantRootish {
				t.Errorf("rootish = %q, want %q", rootish, tc.wantRootish)
			}
		})
	}
}

// TestValidateDatabaseNameLength pins the limits to bytes. MongoDB caps a
// database name at 63 bytes; DumboDB deliberately allows more so that a
// revision-qualified name still fits a usable base name.
func TestValidateDatabaseNameLength(t *testing.T) {
	t.Parallel()

	hash := strings.Repeat("q", 32)

	for _, tc := range []struct {
		name    string
		dbName  string
		wantErr bool
	}{
		{"at the base cap", strings.Repeat("a", MaxDatabaseNameBytes), false},
		{"over the base cap", strings.Repeat("a", MaxDatabaseNameBytes+1), true},
		{"over MongoDB's 63 but under ours", strings.Repeat("a", 100), false},

		// U+00E9 encodes as two bytes, so half the cap in runes is exactly the
		// cap in bytes. A rune-counting check would wrongly accept both rows.
		{"multibyte at the base cap", strings.Repeat("\u00e9", MaxDatabaseNameBytes/2), false},
		{"multibyte over the base cap", strings.Repeat("\u00e9", MaxDatabaseNameBytes/2+1), true},

		{"base at cap with rootish", strings.Repeat("a", MaxDatabaseNameBytes) + "@" + hash, false},
		{"base over cap with rootish", strings.Repeat("a", MaxDatabaseNameBytes+1) + "@" + hash, true},

		{"rootish at cap", "mydb@" + strings.Repeat("r", MaxRootishBytes), false},
		{"rootish over cap", "mydb@" + strings.Repeat("r", MaxRootishBytes+1), true},

		// The cap applies to the decoded rootish, so an encoded form that is
		// over the cap on the wire but under it once decoded is accepted.
		{"rootish over cap only when encoded", "mydb@" + strings.Repeat("%2F", MaxRootishBytes/2), false},

		// An all-digit suffix is part of the database name, not a rootish, so
		// the whole string is held to the base cap.
		{"all-digit suffix counts toward base", strings.Repeat("a", MaxDatabaseNameBytes) + "@1775505756999075683", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateDatabaseName(tc.dbName)
			if tc.wantErr && err == nil {
				t.Errorf("validateDatabaseName(%d bytes) = nil, want error", len(tc.dbName))
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateDatabaseName(%d bytes) = %v, want nil", len(tc.dbName), err)
			}
		})
	}
}
