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
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
)

func validatorForDeferredTest(t *testing.T, minimum int32) *types.Document {
	t.Helper()
	comparison, err := types.NewDocument("$gte", minimum)
	require.NoError(t, err)
	validator, err := types.NewDocument("age", comparison)
	require.NoError(t, err)
	return validator
}

func TestGoverningMetadataConflict(t *testing.T) {
	validator0 := validatorForDeferredTest(t, 0)
	validator3 := validatorForDeferredTest(t, 3)
	validator10 := validatorForDeferredTest(t, 10)
	cases := []struct {
		name               string
		base, ours, theirs *collMeta
		wantDeferred       bool
	}{
		{
			name: "validator diverged",
			base: &collMeta{Validator: validator0}, ours: &collMeta{Validator: validator3},
			theirs: &collMeta{Validator: validator10}, wantDeferred: true,
		},
		{
			name:   "validation level diverged",
			base:   &collMeta{Validator: validator0, ValidationLevel: "strict"},
			ours:   &collMeta{Validator: validator0, ValidationLevel: "moderate"},
			theirs: &collMeta{Validator: validator0, ValidationLevel: "off"}, wantDeferred: true,
		},
		{
			name:   "governing fields changed on different sides",
			base:   &collMeta{Validator: validator0, ValidationAction: "error"},
			ours:   &collMeta{Validator: validator0, ValidationAction: "warn"},
			theirs: &collMeta{Validator: validator3, ValidationAction: "error"}, wantDeferred: true,
		},
		{
			name:   "validation action versus deletion",
			base:   &collMeta{Validator: validator0, ValidationAction: "error"},
			ours:   &collMeta{Validator: validator0, ValidationAction: "warn"},
			theirs: nil, wantDeferred: true,
		},
		{
			name: "one sided validator change",
			base: &collMeta{Validator: validator0}, ours: &collMeta{Validator: validator0},
			theirs: &collMeta{Validator: validator10}, wantDeferred: false,
		},
		{
			name: "identical validator change",
			base: &collMeta{Validator: validator0}, ours: &collMeta{Validator: validator10},
			theirs: &collMeta{Validator: validator10}, wantDeferred: false,
		},
		{
			name: "effective defaults agree",
			base: &collMeta{}, ours: &collMeta{Validator: validator0},
			theirs:       &collMeta{Validator: validator0, ValidationLevel: "strict", ValidationAction: "error"},
			wantDeferred: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := governingMetadataConflict(&metaConflictEntry{base: tc.base, ours: tc.ours, theirs: tc.theirs})
			require.Equal(t, tc.wantDeferred, got)
		})
	}
}

func TestDeferredCollectionNamespaceStateRoundTrip(t *testing.T) {
	names := []string{
		"ordinary_collection",
		deferredCollectionStatePrefix + base64.RawURLEncoding.EncodeToString([]byte("orders")),
		deferredCollectionStatePrefix + base64.RawURLEncoding.EncodeToString([]byte("odd.name/$")),
	}
	conflicts, deferred, err := splitNamespaceState(names)
	require.NoError(t, err)
	require.Equal(t, []string{"ordinary_collection"}, conflicts)
	require.Contains(t, deferred, "orders")
	require.Contains(t, deferred, "odd.name/$")
}

func TestDeferredCollectionNamespaceStateRejectsMalformedMarker(t *testing.T) {
	_, _, err := splitNamespaceState([]string{deferredCollectionStatePrefix + "%%%"})
	require.Error(t, err)
}
