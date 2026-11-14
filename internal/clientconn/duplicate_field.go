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

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
)

// duplicateFieldError returns the error for a command repeating the field at
// path, given as its keys.
func duplicateFieldError(path []string) error {
	return common.DuplicateFieldError(path)
}

func isIndex(key string) bool {
	_, err := strconv.Atoi(key)
	return err == nil
}

// commandDuplicateField returns the keys of the path to the first field a
// command repeats, at any depth, and whether there was one.
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
func commandDuplicateField(doc *types.Document) ([]string, bool) {
	if doc == nil {
		return nil, false
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

	return findDuplicateField(doc, nil, skip, modifierLevel(doc.Command()))
}

// modifierLevel returns, for a command whose update modifiers its handler
// checks for repeated paths itself (write error 40, as on mongod), whether a
// path is that of a modifier's document or lies in an update pipeline.
func modifierLevel(command string) func(path []string) bool {
	switch command {
	case "update":
		// An update pipeline is checked by the handler too, which reports
		// its repeated fields as the statement's write error.
		return func(path []string) bool {
			return len(path) >= 4 && path[0] == "updates" && path[2] == "u" &&
				(strings.HasPrefix(path[3], "$") && len(path) == 4 || isIndex(path[3]))
		}
	case "findAndModify", "findandmodify":
		return func(path []string) bool {
			return len(path) == 2 && path[0] == "update" && strings.HasPrefix(path[1], "$")
		}
	}
	return func([]string) bool { return false }
}

func findDuplicateField(doc *types.Document, prefix []string, skip string, modifier func([]string) bool) ([]string, bool) {
	if doc == nil {
		return nil, false
	}

	if !modifier(prefix) {
		if key, ok := doc.FindDuplicateKey(); ok {
			return join(prefix, key), true
		}
	}

	// Keys and Values are read together rather than calling Get per key:
	// Get scans the fields, which made this walk quadratic in a document's
	// width, and it runs on every command before authentication.
	values := doc.Values()
	for i, key := range doc.Keys() {
		if skip != "" && key == skip {
			continue
		}
		if !isComposite(values[i]) {
			continue
		}
		if path, ok := findDuplicateInValue(values[i], join(prefix, key), modifier); ok {
			return path, true
		}
	}

	return nil, false
}

func findDuplicateInValue(value any, path []string, modifier func([]string) bool) ([]string, bool) {
	switch v := value.(type) {
	case *types.Document:
		return findDuplicateField(v, path, "", modifier)
	case *types.Array:
		for i := 0; i < v.Len(); i++ {
			element, err := v.Get(i)
			if err != nil || !isComposite(element) {
				continue
			}
			if found, ok := findDuplicateInValue(element, join(path, strconv.Itoa(i)), modifier); ok {
				return found, true
			}
		}
	}

	return nil, false
}

func isComposite(value any) bool {
	switch value.(type) {
	case *types.Document, *types.Array:
		return true
	default:
		return false
	}
}

// join returns prefix with key appended, sharing no storage with prefix.
func join(prefix []string, key string) []string {
	return append(prefix[:len(prefix):len(prefix)], key)
}
