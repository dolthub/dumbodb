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

// SessionAwareBackend is the optional interface a Backend implementation
// implements when it holds per-session state (e.g. document locks for
// default-mode MongoDB transactions). The handler invokes OnSessionEnd on
// the `endSessions` command so the backend can release that state.
//
// Modeled on VersioningBackend: handlers type-assert the underlying
// Backend to this interface and call only when supported. Backends that
// hold no per-session state (the stub backend) need not implement it.
type SessionAwareBackend interface {
	// OnSessionEnd is called when the Mongo client signals that a session
	// has ended (explicit endSessions or implicit timeout). The owner is
	// the same identifier conninfo.ConnInfo.Owner() returns: the lsid hex
	// string when the connection carries one, or a synthetic conn:0xADDR
	// id otherwise.
	//
	// Implementations must be idempotent and safe to call for owners that
	// hold no state.
	OnSessionEnd(owner string)
}
