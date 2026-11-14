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

package stages

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// validateExpression recursively validates expressions in document and array
// returns error when there is unsupported expression present.
// Currently, it raises ErrNotImplemented if there is any expression.
// Command Errors:
//   - ErrNotImplemented
func validateExpression(stage string, doc *types.Document) error {
	iter := doc.Iterator()
	defer iter.Close()

	for {
		_, v, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return nil
		}

		if err != nil {
			return lazyerrors.Error(err)
		}

		switch value := v.(type) {
		case *types.Document:
			if err := validateExpression(stage, value); err != nil {
				return err
			}
		case *types.Array:
			if err := validateArrayExpression(stage, value); err != nil {
				return err
			}
		}
	}
}

// validateArrayExpression validates each document in array arr and
// returns error when there is unsupported expression present in any document.
// Currently, it raises error if there is any expression.
// Command Errors:
//   - ErrNotImplemented
func validateArrayExpression(stage string, arr *types.Array) error {
	iter := arr.Iterator()
	defer iter.Close()

	for {
		_, v, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return nil
		}

		if err != nil {
			return lazyerrors.Error(err)
		}

		doc, ok := v.(*types.Document)
		if !ok {
			continue
		}

		if err := validateExpression(stage, doc); err != nil {
			return err
		}
	}
}

// validateFieldPath checks the field names of a $set or $addFields stage.
func validateFieldPath(stage string, fieldsDoc *types.Document) error {
	for _, key := range fieldsDoc.Keys() {
		if err := checkFieldPath(stage, key); err != nil {
			return err
		}
	}

	return nil
}

// checkFieldPath returns the error mongod's FieldPath raises for field, in
// the order it checks.
func checkFieldPath(stage, field string) error {
	fail := func(code handlererrors.ErrorCode, reason string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code,
			fmt.Sprintf("Invalid %s :: caused by :: %s", stage, reason), stage+" (stage)")
	}

	switch {
	case field == "":
		return fail(handlererrors.ErrEmptyFieldPath, "FieldPath cannot be constructed with empty string")
	case strings.HasSuffix(field, "."):
		return fail(handlererrors.ErrInvalidFieldPath, "FieldPath must not end with a '.'.")
	}

	for _, name := range strings.Split(field, ".") {
		switch {
		case name == "":
			return fail(handlererrors.ErrPathContainsEmptyElement, "FieldPath field names may not be empty strings.")
		case strings.HasPrefix(name, "$"):
			return fail(handlererrors.ErrFieldPathInvalidName,
				"FieldPath field names may not start with '$'. Consider using $getField or $setField.")
		}
	}

	return nil
}

// pathCollision returns the first path of paths that equals, or lies within
// or around, an earlier one, and that earlier path.
func pathCollision(paths []string) (string, string, bool) {
	full := map[string]bool{}
	prefixes := map[string]string{}
	for _, p := range paths {
		if full[p] {
			return p, p, true
		}
		if other, ok := prefixes[p]; ok {
			return p, other, true
		}
		for i := range len(p) {
			if p[i] == '.' && full[p[:i]] {
				return p, p[:i], true
			}
		}
		full[p] = true
		for i := range len(p) {
			if p[i] == '.' {
				if _, ok := prefixes[p[:i]]; !ok {
					prefixes[p[:i]] = p
				}
			}
		}
	}
	return "", "", false
}

// checkAddFieldsCollisions returns mongod's error for a $set or $addFields
// specification that writes one path within another.
func checkAddFieldsCollisions(stage string, spec *types.Document) error {
	if name, ok := dottedSubObjectField(spec, true); ok {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDottedFieldInSubObject,
			fmt.Sprintf("Invalid %s :: caused by :: cannot use dotted field name '%s' in a sub object: %s",
				stage, name, types.FormatAnyValue(spec)), stage+" (stage)")
	}
	path, other, ok := pathCollision(addFieldsPaths("", spec))
	if !ok {
		return nil
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrAddFieldsPathCollision,
		fmt.Sprintf("Invalid %s :: caused by :: specification contains two conflicting paths. Cannot specify both '%s' and '%s': %s",
			stage, path, other, types.FormatAnyValue(spec)), stage+" (stage)")
}

// dottedSubObjectField returns the first dotted field name inside an object
// literal of a $set or $addFields specification.
func dottedSubObjectField(spec *types.Document, top bool) (string, bool) {
	for _, k := range spec.Keys() {
		if !top && strings.Contains(k, ".") {
			return k, true
		}
		if d, ok := must.NotFail(spec.Get(k)).(*types.Document); ok && !operators.IsOperator(d) {
			if name, found := dottedSubObjectField(d, false); found {
				return name, true
			}
		}
	}
	return "", false
}

// addFieldsPaths returns the paths a $set or $addFields specification
// writes, with the fields of nested object literals expanded; an empty
// object writes none.
func addFieldsPaths(prefix string, spec *types.Document) []string {
	var paths []string
	for _, k := range spec.Keys() {
		path := prefix + k
		if d, ok := must.NotFail(spec.Get(k)).(*types.Document); ok && !operators.IsOperator(d) {
			paths = append(paths, addFieldsPaths(path+".", d)...)
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// checkFieldPathArgument returns mongod's FieldPath error for a stage
// argument that names a field path.
func checkFieldPathArgument(path, stage string) error {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, stage+" (stage)")
	}
	const hint = "Consider using $getField or $setField for a field path with '.' or '$'. :: caused by :: "
	switch {
	case path == "":
		return fail(handlererrors.ErrEmptyFieldPath, "FieldPath cannot be constructed with empty string")
	case strings.HasSuffix(path, "."):
		return fail(handlererrors.ErrInvalidFieldPath, "FieldPath must not end with a '.'.")
	}
	for _, name := range strings.Split(path, ".") {
		switch {
		case name == "":
			return fail(handlererrors.ErrPathContainsEmptyElement, hint+"FieldPath field names may not be empty strings.")
		case strings.HasPrefix(name, "$"):
			return fail(handlererrors.ErrFieldPathInvalidName, hint+"FieldPath field names may not start with '$', given '"+name+"'.")
		case strings.ContainsRune(name, 0):
			return fail(handlererrors.ErrFieldPathNulByte, hint+"FieldPath field names may not contain '"+name+"'.")
		}
	}
	return nil
}

// checkTargetNamespace returns mongod's error for a $out or $merge target
// whose database or collection name holds a NUL byte.
func checkTargetNamespace(db, coll, stage string) error {
	switch {
	case strings.ContainsRune(db, 0):
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace,
			"namespace cannot have embedded null characters", stage+" (stage)")
	case strings.ContainsRune(coll, 0):
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace,
			"namespaces cannot have embedded null characters", stage+" (stage)")
	}
	return nil
}
