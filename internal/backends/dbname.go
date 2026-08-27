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
	"net/url"
	"strings"
)

// DBRootishSep separates the database name from the rootish in an encoded
// database name (e.g. "mydb@main").
const DBRootishSep = "@"

// MaxDatabaseNameBytes bounds the base database name, in bytes rather than
// characters because every constraint below this layer is measured in bytes.
//
// The base name is used verbatim as a directory name. NAME_MAX is 255 bytes,
// and Windows caps a whole path at 260 by default, so this leaves room for the
// data directory prefix and the paths DumboDB creates beneath the database
// directory. MongoDB's own limit is 63 bytes; exceeding it is a deliberate
// deviation covered by a DumboDBDeviates parity case.
const MaxDatabaseNameBytes = 128

// MaxRootishBytes bounds the decoded rootish. Dolt imposes no length limit on
// ref names, so this is DumboDB policy rather than a storage constraint.
const MaxRootishBytes = 512

// SplitEncodedDBName splits an encoded database name "dbname@rootish" into the
// base name and the percent-decoded rootish. The rootish is empty when the name
// carries none.
//
// Percent-decoding lets clients express branch names containing characters that
// are invalid in a MongoDB database name ("feature%2Ffoo" -> "feature/foo"). A
// decode failure yields the raw value; callers that must reject bad encoding
// check for it themselves.
//
// An all-digit suffix (e.g. a Unix nanosecond timestamp) is not a valid rootish,
// so "prefix@1775505756999075683" splits as a plain database name carrying no
// rootish.
func SplitEncodedDBName(encoded string) (dbName, rootish string) {
	idx := strings.Index(encoded, DBRootishSep)
	if idx <= 0 {
		return encoded, ""
	}

	raw := encoded[idx+len(DBRootishSep):]
	candidate := raw
	if decoded, err := url.PathUnescape(raw); err == nil {
		candidate = decoded
	}

	if rootishAllDigits(candidate) {
		return encoded, ""
	}

	return encoded[:idx], candidate
}

// rootishAllDigits reports whether s is entirely ASCII digits and is not a
// commit hash. Dolt hashes are exactly 32 base32 characters (0-9a-v), so a
// 32-digit string is a valid hash and must still be treated as a rootish.
func rootishAllDigits(s string) bool {
	if s == "" || len(s) == 32 {
		return false
	}

	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}

	return true
}
