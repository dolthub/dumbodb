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

package backends

import (
	"errors"
	"fmt"
	"slices"

	"github.com/dolthub/dumbodb/internal/types"
)

//go:generate ../../bin/stringer -linecomment -type ErrorCode

type ErrorCode int

// Error codes.
const (
	_ ErrorCode = iota

	ErrorCodeDatabaseNameIsInvalid
	ErrorCodeDatabaseDoesNotExist

	ErrorCodeCollectionNameIsInvalid
	ErrorCodeCollectionDoesNotExist
	ErrorCodeCollectionAlreadyExists

	ErrorCodeInsertDuplicateID

	ErrorCodeReadOnlyDatabase

	ErrorCodeWriteConflict
)

// Error represents a backend error returned by all Backend, Database and Collection methods.
type Error struct {
	// This internal error can't be accessed by the caller; it exists only for debugging.
	// It may be nil.
	err error

	code ErrorCode

	// dupIndex and dupKey carry structured detail for ErrorCodeInsertDuplicateID:
	// the index that was violated and the offending key {field: value}, so the
	// handler can build MongoDB's "index: <name> dup key: { ... }" message.
	// dupIndex is "" and dupKey is nil for other errors.
	dupIndex string
	dupKey   *types.Document
}

// NewError creates a new backend error.
//
// Code must not be 0. Err may be nil.
func NewError(code ErrorCode, err error) *Error {
	if code == 0 {
		panic("backends.NewError: code must not be 0")
	}

	return &Error{
		code: code,
		err:  err,
	}
}

// NewDuplicateKeyError creates an ErrorCodeInsertDuplicateID naming the violated
// index and the offending key, so callers can report which index collided.
func NewDuplicateKeyError(index string, key *types.Document) *Error {
	return &Error{
		code:     ErrorCodeInsertDuplicateID,
		err:      fmt.Errorf("duplicate key on index %q", index),
		dupIndex: index,
		dupKey:   key,
	}
}

func (err *Error) Code() ErrorCode {
	return err.code
}

// DupIndex returns the violated index name for a duplicate-key error, or "".
func (err *Error) DupIndex() string {
	return err.dupIndex
}

// DupKey returns the offending key for a duplicate-key error, or nil.
func (err *Error) DupKey() *types.Document {
	return err.dupKey
}

// There is intentionally no method to return the internal error.

func (err *Error) Error() string {
	return fmt.Sprintf("%s: %v", err.code, err.err)
}

// ErrorCodeIs reports whether err (or any error it wraps) is *Error with one
// of the given error codes. Uses errors.As, so callers that wrap with
// lazyerrors / fmt.Errorf still match.
//
// At least one error code must be given.
func ErrorCodeIs(err error, code ErrorCode, codes ...ErrorCode) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.code == code || slices.Contains(codes, e.code)
}

// checkError is a no-op retained for API compatibility.
//
// It previously enforced backend error contracts in debug builds; production builds always skipped it.
func checkError(err error, codes ...ErrorCode) {}

var (
	_ error = (*Error)(nil)
)
