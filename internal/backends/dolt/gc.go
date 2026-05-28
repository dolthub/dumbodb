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

package dolt

import (
	"context"
	"fmt"

	"github.com/dolthub/dolt/go/store/hash"
)

// backgroundGCRootsProvider is the GCRootsProvider used by
// RunUnderGCSafepointKeeper to bracket background mutators (capped
// collection cleanup today; future GC-triggered tasks tomorrow). It
// holds no per-session state and reports no extra roots: every chunk
// the background tick writes goes through updateBranchWS, which fsyncs
// the on-disk working_set ref before the chunk is otherwise reachable.
// GC's standard disk-ref walk covers all of it.
//
// The bracket exists not for root accounting but to gate GC's
// pre-finalize safepoint on the tick's completion, so chunk-store
// rewrites do not race the tick's in-flight ops.
type backgroundGCRootsProvider struct{}

func (*backgroundGCRootsProvider) VisitGCRoots(_ context.Context, _ string, _ func(hash.Hash) bool) error {
	return nil
}

// RunUnderGCSafepointKeeper invokes fn while registered with the
// backend's GCSafepointController. An in-flight GC sweep blocks at its
// pre-finalize safepoint until SessionCommandEnd fires for the
// keeper's rootsProvider; new GC sweeps started while the bracket is
// active will likewise wait. Implements backends.GCSafepointBackend.
func (b *Backend) RunUnderGCSafepointKeeper(_ context.Context, fn func() error) error {
	if b.gcController == nil || b.backgroundRP == nil {
		return fn()
	}
	if err := b.gcController.SessionCommandBegin(b.backgroundRP); err != nil {
		return fmt.Errorf("RunUnderGCSafepointKeeper: SessionCommandBegin: %w", err)
	}
	defer b.gcController.SessionCommandEnd(b.backgroundRP)
	return fn()
}
