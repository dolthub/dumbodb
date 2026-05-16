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

package common

import (
	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
)

// mongoWriteConflictCode is the MongoDB wire-protocol error code 112,
// surfaced when a default-mode transaction conflicts with another on a
// document-level pessimistic lock. Defined inline rather than as a named
// constant in handlererrors because the generated stringer table covers
// only the codes the handler emits directly today.
const mongoWriteConflictCode = handlererrors.ErrorCode(112)

// TranslateBackendWriteError maps backend-level write errors (returned
// from InsertAll / UpdateAll / DeleteAll) into wire-protocol command
// errors with the matching MongoDB error code. Callers should invoke it
// on any write-path error before returning to the wire layer.
//
// Returns nil if err is nil. Returns err unchanged for codes the
// function does not translate; the caller decides what to do (typically
// lazyerrors.Error wrapping or per-write-error accumulation).
func TranslateBackendWriteError(err error) error {
	if err == nil {
		return nil
	}
	if backends.ErrorCodeIs(err, backends.ErrorCodeWriteConflict) {
		return handlererrors.NewCommandError(mongoWriteConflictCode, err)
	}
	return err
}
