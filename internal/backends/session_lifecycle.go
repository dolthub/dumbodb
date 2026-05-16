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

import "context"

// SessionAwareBackend is the optional interface a Backend implementation
// implements when it holds per-session state (e.g. document locks for
// default-mode MongoDB transactions). The handler invokes these methods
// on the `endSessions` / `commitTransaction` / `abortTransaction`
// commands so the backend can manage that state.
//
// Modeled on VersioningBackend: handlers type-assert the underlying
// Backend to this interface and call only when supported. Backends that
// hold no per-session state (the stub backend) need not implement it.
//
// All methods take an owner string -- the identifier
// conninfo.ConnInfo.Owner() returns (lsid hex when the connection
// carries one, synthetic conn:0xADDR otherwise). Implementations must
// be idempotent and safe to call for owners that hold no state.
type SessionAwareBackend interface {
	// OnSessionEnd is called when the Mongo client signals that a session
	// has ended (explicit endSessions or implicit timeout). The backend
	// drops any per-session state, including uncommitted transaction
	// working-set overlays and document locks held by the owner.
	OnSessionEnd(owner string)

	// OnTransactionCommit merges the owner's pending per-transaction
	// overlay back into the committed working set across every (owner,
	// branch) entry, persists the result, and releases any document
	// locks the owner holds. Called by the handler on a successful
	// commitTransaction.
	OnTransactionCommit(ctx context.Context, owner string) error

	// OnTransactionAbort discards the owner's pending per-transaction
	// overlay without touching the committed working set, and releases
	// any document locks the owner holds. Called by the handler on
	// abortTransaction.
	OnTransactionAbort(owner string)
}
