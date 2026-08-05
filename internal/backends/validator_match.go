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

// validatorMatcher holds the predicate used to evaluate a document validator.
// The handler registers common.FilterDocument here at process init, letting the
// backend enforce validators during a merge without importing handler/common
// (which would create a circular dependency).
var validatorMatcher atomic.Pointer[func(doc, validator *types.Document) (bool, error)]

// RegisterValidatorMatcher installs fn as the validator predicate. The handler
// must call this once at process init before any backend performs a merge.
func RegisterValidatorMatcher(fn func(doc, validator *types.Document) (bool, error)) {
	validatorMatcher.Store(&fn)
}

// DocumentSatisfiesValidator reports whether doc conforms to validator. It
// returns an error if no predicate has been registered.
func DocumentSatisfiesValidator(doc, validator *types.Document) (bool, error) {
	fn := validatorMatcher.Load()
	if fn == nil {
		return false, fmt.Errorf("backends: validator matcher not registered")
	}
	return (*fn)(doc, validator)
}
