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
	"fmt"
	"sync/atomic"

	"github.com/dolthub/dumbodb/internal/types"
)

// partialFilterMatcher holds the function used to evaluate a partial-index
// filter expression against a document. The handler layer registers
// common.FilterDocument here at process init so that the backend can rebuild
// IndexInfo.MatchesPartialFilter closures (e.g. after restart from disk)
// without importing handler/common (which would create a circular dependency).
//
// The registered function takes (doc, filter) — same order as
// common.FilterDocument — so it can be passed by value without a wrapper.
var partialFilterMatcher atomic.Pointer[func(doc, filter *types.Document) (bool, error)]

// RegisterPartialFilterMatcher installs fn as the global predicate used to
// evaluate partial-index filter expressions. The handler layer must call this
// once at process init before any backend opens existing data.
func RegisterPartialFilterMatcher(fn func(doc, filter *types.Document) (bool, error)) {
	partialFilterMatcher.Store(&fn)
}

// MatchPartialFilter reports whether doc satisfies filter, using the
// registered predicate. It returns an error if no predicate has been
// registered, which indicates a misconfigured binary (handler/common was not
// linked in).
func MatchPartialFilter(doc, filter *types.Document) (bool, error) {
	fn := partialFilterMatcher.Load()
	if fn == nil {
		return false, fmt.Errorf("backends: partial filter matcher not registered")
	}
	return (*fn)(doc, filter)
}
