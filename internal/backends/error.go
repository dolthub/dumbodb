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
	ErrorCodeDatabaseDifferCase

	ErrorCodeCollectionNameIsInvalid
	ErrorCodeCollectionDoesNotExist
	ErrorCodeCollectionAlreadyExists

	ErrorCodeInsertDuplicateID

	ErrorCodeReadOnlyDatabase

	ErrorCodeWriteConflict

	ErrorCodeReadOnlyCollection

	// ErrorCodeBranchInConflict refuses a write to a branch that has an
	// operation paused on conflicts.
	ErrorCodeBranchInConflict
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

	// existingDB and requestedDB name the databases of an
	// ErrorCodeDatabaseDifferCase error; both are "" for other errors.
	existingDB  string
	requestedDB string
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

// NewDatabaseDifferCaseError creates an ErrorCodeDatabaseDifferCase for a
// requested database name that differs only in case from an existing one.
func NewDatabaseDifferCaseError(existing, requested string) *Error {
	return &Error{
		code:        ErrorCodeDatabaseDifferCase,
		err:         fmt.Errorf("database %q differs only in case from %q", requested, existing),
		existingDB:  existing,
		requestedDB: requested,
	}
}

// DifferCaseNames returns the existing and requested database names of an
// ErrorCodeDatabaseDifferCase error.
func (err *Error) DifferCaseNames() (existing, requested string) {
	return err.existingDB, err.requestedDB
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

// Message is the error's description without its code.
func (err *Error) Message() string {
	if err.err == nil {
		return err.code.String()
	}
	return err.err.Error()
}

func (err *Error) Error() string {
	return fmt.Sprintf("%s: %v", err.code, err.err)
}

// ErrWriteRaced reports that a write could not be published because the branch
// moved after the write read it. It is not a failure and not a refusal: the
// write is still valid, it just has to be reconciled against the new tip and
// tried again. Callers match it with errors.Is.
var ErrWriteRaced = errors.New("write raced another publish on this branch")

// ErrUncommittedChanges reports that a merge, cherry-pick, revert or rebase
// refused to start because the branch has uncommitted changes. A server's
// working set is shared by every session, so these operations do not carry
// uncommitted changes through, unlike git; the client commits or resets them
// first. Callers match it with errors.Is.
var ErrUncommittedChanges = errors.New("the branch has uncommitted changes")

// ErrOperationPaused reports a command or write refused because a merge,
// cherry-pick, revert or rebase is paused on conflicts on its branch. Callers
// match it with errors.Is.
var ErrOperationPaused = errors.New("an operation is paused on conflicts")

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
