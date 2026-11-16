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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// The handler layer registers the real canonicalizer; these tests use filters
// already in canonical form.
func init() {
	RegisterFilterCanonicalizer(func(filter *types.Document) *types.Document { return filter })
}

func TestUnregisteredPartialFilterHooksFail(t *testing.T) {
	canonicalizer, compiler := filterCanonicalizer, partialFilterCompiler
	t.Cleanup(func() { filterCanonicalizer, partialFilterCompiler = canonicalizer, compiler })
	filterCanonicalizer, partialFilterCompiler = nil, nil

	pfe := must.NotFail(types.NewDocument("a", int32(1)))
	_, err := CanonicalPartialFilter(pfe)
	require.ErrorContains(t, err, "not registered")

	_, err = PartialFilterMembership(pfe, nil)(must.NotFail(types.NewDocument("a", int32(1))))
	require.ErrorContains(t, err, "not registered")
}
