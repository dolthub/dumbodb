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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
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
