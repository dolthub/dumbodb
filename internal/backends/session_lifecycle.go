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
	"context"

	"github.com/dolthub/dumbodb/internal/sqlctx"
)

type SessionAwareBackend interface {
	OnSessionEnd(owner string)
	OnTransactionCommit(ctx context.Context, owner string) error
	OnTransactionAbort(owner string)
	SessionIsolation() bool
	SessionRegistry() *sqlctx.SessionRegistry
}

// AutoCommitBackend commits a branch's working root at the command boundary
// under --auto-commit, reporting whether a commit was created.
type AutoCommitBackend interface {
	AutoCommit(ctx context.Context, dbName, branch, message string) (bool, error)
}
