// Copyright 2021 FerretDB Inc.
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

package common

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// DuplicateFieldError returns the error for a command repeating the field at
// path, given as its keys: mongod's error where it parses the repeated keys
// as paths or sort keys, and FailedToParse otherwise.
func DuplicateFieldError(path []string) error {
	last := path[len(path)-1]

	// The innermost pipeline stage holding the repeated key.
	stage := -1
	for k := len(path) - 1; k > 0; k-- {
		if isIndex(path[k-1]) && strings.HasPrefix(path[k], "$") {
			stage = k
			break
		}
	}

	if stage >= 0 {
		name, rest := path[stage], path[stage+1:]
		switch {
		case (name == "$set" || name == "$addFields") && isFieldPath(rest):
			p := strings.Join(rest, ".")
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrAddFieldsPathCollision,
				fmt.Sprintf("Invalid %s :: caused by :: specification contains two conflicting paths. Cannot specify both '%s' and '%s'",
					name, p, p), name+" (stage)")
		case name == "$project" && isFieldPath(rest):
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrUnsetPathOverwrite,
				"Invalid $project :: caused by :: Path collision at "+last, "$project (stage)")
		case name == "$sort" && len(rest) == 1:
			return sortDuplicateError(last)
		case name == "$listClusterCatalog" && len(rest) == 1:
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIDLDuplicateField,
				fmt.Sprintf("BSON field '$listClusterCatalog.%s' is a duplicate field", last), "$listClusterCatalog (stage)")
		case name == "$setWindowFields" && len(rest) == 2 && rest[0] == "output":
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrWindowOutputPathCollision,
				"$setWindowFields 'output' specification contains two conflicting paths", "$setWindowFields (stage)")
		}
	} else {
		switch {
		case len(path) == 2 && path[0] == "sort":
			return sortDuplicateError(last)
		case len(path) >= 2 && path[0] == "projection" && isFieldPath(path[1:]):
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrUnsetPathOverwrite,
				"Path collision at "+last, "projection")
		}
	}

	return handlererrors.NewCommandErrorMsg(handlererrors.ErrFailedToParse,
		fmt.Sprintf("BSON field '%s' is a duplicate field", strings.Join(path, ".")))
}

func sortDuplicateError(key string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortDuplicateKey,
		fmt.Sprintf("$sort key must not contain duplicate keys (duplicate: '%s')", key), "$sort")
}

// isFieldPath reports whether keys name a path of fields, entering no
// operator or array.
func isFieldPath(keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "$") || isIndex(k) {
			return false
		}
	}
	return true
}

func isIndex(key string) bool {
	_, err := strconv.Atoi(key)
	return err == nil
}

// findDuplicatePath returns the keys of the path, below prefix, to the first
// field value repeats at any depth, and whether there was one.
func findDuplicatePath(value any, prefix []string) ([]string, bool) {
	switch v := value.(type) {
	case *types.Document:
		if key, ok := v.FindDuplicateKey(); ok {
			return append(prefix[:len(prefix):len(prefix)], key), true
		}
		values := v.Values()
		for i, key := range v.Keys() {
			if path, ok := findDuplicatePath(values[i], append(prefix[:len(prefix):len(prefix)], key)); ok {
				return path, true
			}
		}
	case *types.Array:
		for i := range v.Len() {
			if path, ok := findDuplicatePath(must.NotFail(v.Get(i)), append(prefix[:len(prefix):len(prefix)], strconv.Itoa(i))); ok {
				return path, true
			}
		}
	}
	return nil, false
}
