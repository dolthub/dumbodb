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

// GCSafepointBackend is an optional interface backends may implement to
// participate in GC's keeper-driven safepoint protocol. Background
// mutators that run outside the wire-dispatch path (e.g., capped
// collection cleanup) wrap their work via RunUnderGCSafepointKeeper so
// an in-flight GC sweep waits for the mutator to release before
// swapping chunk stores.
//
// Backends that do not implement GC (the stub backend, for instance)
// can skip implementing this interface; callers fall back to invoking
// fn directly.
type GCSafepointBackend interface {
	RunUnderGCSafepointKeeper(ctx context.Context, fn func() error) error
}
