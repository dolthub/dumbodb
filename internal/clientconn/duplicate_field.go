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

package clientconn

import (
	"strconv"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
)

// commandDuplicateField returns the dotted path of the first field a command
// repeats, at any depth, and whether there was one.
//
// BSON lets a document repeat a key, so this is reachable from any client at
// any position: a driver attaching its own lsid to a command that already
// carries one, or a filter written twice. Such a document is not safe to read.
// types.Document.Get panics on a duplicated key, so a handler that happens to
// read the repeated field takes the connection down, and one that does not
// read it silently accepts a command with a value nobody chose.
//
// The walk is over an already-materialized document, so it adds no decoding
// and cannot recurse deeper than the decoder already did.
func commandDuplicateField(doc *types.Document) (string, bool) {
	if doc == nil {
		return "", false
	}

	// One exception, and only at the top level. An insert reports a duplicate
	// inside a document it was asked to store as a per-document write error,
	// so a batch of a hundred fails only the malformed one. Rejecting the
	// whole command here would be a worse answer, and the payload is already
	// checked before anything reads it.
	skip := ""
	if strings.EqualFold(doc.Command(), "insert") {
		skip = "documents"
	}

	return findDuplicateField(doc, "", skip)
}

func findDuplicateField(doc *types.Document, prefix, skip string) (string, bool) {
	if doc == nil {
		return "", false
	}

	if key, ok := doc.FindDuplicateKey(); ok {
		return join(prefix, key), true
	}

	for _, key := range doc.Keys() {
		if skip != "" && key == skip {
			continue
		}
		value, err := doc.Get(key)
		if err != nil {
			continue
		}
		if path, ok := findDuplicateInValue(value, join(prefix, key)); ok {
			return path, true
		}
	}

	return "", false
}

func findDuplicateInValue(value any, path string) (string, bool) {
	switch v := value.(type) {
	case *types.Document:
		return findDuplicateField(v, path, "")
	case *types.Array:
		for i := 0; i < v.Len(); i++ {
			element, err := v.Get(i)
			if err != nil {
				continue
			}
			indexed := path + "." + strconv.Itoa(i)
			if found, ok := findDuplicateInValue(element, indexed); ok {
				return found, true
			}
		}
	}

	return "", false
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}
