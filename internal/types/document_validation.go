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

package types

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dolthub/dumbodb/internal/util/must"
)

//go:generate ../../bin/stringer -linecomment -type ValidationErrorCode

type ValidationErrorCode int

const (
	_ ValidationErrorCode = iota
	ErrValidation
	ErrWrongIDType
	ErrIDNotFound
	ErrDollarPrefixedID
)

// ValidationError describes an error that could occur when validating a document.
type ValidationError struct {
	code   ValidationErrorCode
	reason error
}

func newValidationError(code ValidationErrorCode, reason error) error {
	return &ValidationError{reason: reason, code: code}
}

func (e *ValidationError) Error() string {
	return e.reason.Error()
}

func (e *ValidationError) Code() ValidationErrorCode {
	return e.code
}

// ValidateData checks if the document represents a valid "data document".
// It places `_id` field into the fields slice 0 index.
// If the document is not valid it returns *ValidationError.
func (d *Document) ValidateData() error {
	return d.validateData(true)
}

// validateData applies different validation rules to the `_id` field depending on the document level.
func (d *Document) validateData(isTopLevel bool) error {
	d.moveIDToTheFirstIndex()

	keys := d.Keys()
	values := d.Values()

	duplicateChecker := make(map[string]struct{}, len(keys))
	var idPresent bool

	for i, key := range keys {
		if !utf8.ValidString(key) {
			return newValidationError(ErrValidation, fmt.Errorf("invalid key: %q (not a valid UTF-8 string)", key))
		}

		if _, ok := duplicateChecker[key]; ok {
			if key == "_id" {
				return newValidationError(ErrValidation, fmt.Errorf("can't have multiple _id fields in one document"))
			}

			return newValidationError(ErrValidation, fmt.Errorf("invalid key: %q (duplicate keys are not allowed)", key))
		}
		duplicateChecker[key] = struct{}{}

		if key == "_id" {
			idPresent = true
		}

		value := values[i]

		switch value := value.(type) {
		case *Document:
			if isTopLevel && key == "_id" {
				if err := validateNoDollarInID(value); err != nil {
					return err
				}
			}

			err := value.validateData(false)
			if err != nil {
				var vErr *ValidationError

				if errors.As(err, &vErr) && vErr.code == ErrIDNotFound {
					continue
				}

				return err
			}
		case *Array:
			if isTopLevel && key == "_id" {
				return newValidationError(ErrWrongIDType, fmt.Errorf("The '_id' value cannot be of type array"))
			}

			for i := 0; i < value.Len(); i++ {
				if doc, ok := must.NotFail(value.Get(i)).(*Document); ok {
					err := doc.validateData(false)
					if err != nil {
						var vErr *ValidationError

						if errors.As(err, &vErr) && vErr.code == ErrIDNotFound {
							continue
						}

						return err
					}
				}
			}
		case Regex:
			if isTopLevel && key == "_id" {
				return newValidationError(ErrWrongIDType, fmt.Errorf("The '_id' value cannot be of type regex"))
			}
		}
	}

	if !idPresent {
		return newValidationError(ErrIDNotFound, fmt.Errorf("invalid document: document must contain '_id' field"))
	}

	return nil
}

// validateNoDollarInID rejects a document _id that contains a $-prefixed field
// name at any depth, matching MongoDB, which permits $-prefixed names only when
// a subdocument is a valid DBRef ({$ref, $id[, $db]}).
func validateNoDollarInID(doc *Document) error {
	dbRef := isDBRef(doc)
	keys := doc.Keys()
	values := doc.Values()

	for i, key := range keys {
		if !dbRef && strings.HasPrefix(key, "$") {
			// %s (not %q) reproduces MongoDB's message verbatim; parity tests
			// compare on the error code.
			return newValidationError(ErrDollarPrefixedID, fmt.Errorf(
				"_id fields may not contain '$'-prefixed fields: %s is not valid for storage.", key))
		}

		switch value := values[i].(type) {
		case *Document:
			if err := validateNoDollarInID(value); err != nil {
				return err
			}
		case *Array:
			for j := 0; j < value.Len(); j++ {
				if nested, ok := must.NotFail(value.Get(j)).(*Document); ok {
					if err := validateNoDollarInID(nested); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// isDBRef reports whether doc is a DBRef: first field $ref (string), second
// field $id, an optional $db (string), and no other $-prefixed fields. MongoDB
// exempts this shape from the _id $-prefix restriction.
func isDBRef(doc *Document) bool {
	keys := doc.Keys()
	values := doc.Values()

	if len(keys) < 2 || keys[0] != "$ref" || keys[1] != "$id" {
		return false
	}

	if _, ok := values[0].(string); !ok {
		return false
	}

	for i := 2; i < len(keys); i++ {
		if keys[i] == "$db" {
			if _, ok := values[i].(string); !ok {
				return false
			}
			continue
		}

		if strings.HasPrefix(keys[i], "$") {
			return false
		}
	}

	return true
}
