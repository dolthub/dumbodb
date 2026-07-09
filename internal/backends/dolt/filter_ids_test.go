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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func hashFor(t *testing.T, idVal any) string {
	t.Helper()
	h, err := hashID(idVal)
	require.NoError(t, err)
	return hashFromArray(h).String()
}

func TestResolvePreciseScalarID(t *testing.T) {
	cases := []struct {
		name string
		id   any
	}{
		{"int32", int32(42)},
		{"int64", int64(42)},
		{"float64", 3.14},
		{"string", "hello"},
		{"bool", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			filter := must.NotFail(types.NewDocument("_id", c.id))
			ids, full, err := ResolveFilterToDocIDHashes(filter)
			require.NoError(t, err)
			assert.False(t, full, "scalar _id must yield a precise lock set")
			require.Len(t, ids, 1)
			assert.Equal(t, hashFor(t, c.id), ids[0].String())
		})
	}
}

func TestHashIDNumericCanonicalization(t *testing.T) {
	int42 := hashFor(t, int32(42))
	assert.Equal(t, int42, hashFor(t, int64(42)), "int64(42) must hash as int32(42)")
	assert.Equal(t, int42, hashFor(t, float64(42)), "double(42.0) must hash as int32(42)")

	zero := hashFor(t, int64(0))
	assert.Equal(t, zero, hashFor(t, float64(0)), "double(0.0) must hash as int64(0)")
	assert.Equal(t, zero, hashFor(t, math.Copysign(0, -1)), "double(-0.0) must hash as int64(0)")

	assert.NotEqual(t, int42, hashFor(t, float64(42.5)), "42.5 must not hash as 42")
	assert.NotEqual(t, int42, hashFor(t, "42"), "string \"42\" must not hash as numeric 42")

	big := 1e300
	assert.Equal(t, hashFor(t, big), hashFor(t, big), "large double hashes stably")
	assert.NotEqual(t, hashFor(t, big), hashFor(t, int64(0)))
	assert.NotEqual(t, hashFor(t, math.Inf(1)), int42)
}

func TestResolveInArrayOfIDs(t *testing.T) {
	in := must.NotFail(types.NewArray(int32(1), int32(2), int32(3)))
	filter := must.NotFail(types.NewDocument(
		"_id", must.NotFail(types.NewDocument("$in", in)),
	))

	ids, full, err := ResolveFilterToDocIDHashes(filter)
	require.NoError(t, err)
	assert.False(t, full)
	require.Len(t, ids, 3)

	assert.Equal(t, hashFor(t, int32(1)), ids[0].String())
	assert.Equal(t, hashFor(t, int32(2)), ids[1].String())
	assert.Equal(t, hashFor(t, int32(3)), ids[2].String())
}

func TestResolveFullCollectionFallbacks(t *testing.T) {
	cases := []struct {
		name   string
		filter *types.Document
	}{
		{
			name:   "nil filter",
			filter: nil,
		},
		{
			name:   "empty filter",
			filter: must.NotFail(types.NewDocument()),
		},
		{
			name: "filter on non-_id field",
			filter: must.NotFail(types.NewDocument(
				"name", "alice",
			)),
		},
		{
			name: "_id range operator $gt",
			filter: must.NotFail(types.NewDocument(
				"_id", must.NotFail(types.NewDocument("$gt", int32(5))),
			)),
		},
		{
			name: "_id range operator $gte",
			filter: must.NotFail(types.NewDocument(
				"_id", must.NotFail(types.NewDocument("$gte", int32(5))),
			)),
		},
		{
			name: "_id compound operators",
			filter: must.NotFail(types.NewDocument(
				"_id", must.NotFail(types.NewDocument(
					"$gt", int32(1),
					"$lt", int32(10),
				)),
			)),
		},
		{
			name: "_id $in with a non-array value",
			filter: must.NotFail(types.NewDocument(
				"_id", must.NotFail(types.NewDocument("$in", "not-an-array")),
			)),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ids, full, err := ResolveFilterToDocIDHashes(c.filter)
			require.NoError(t, err)
			assert.True(t, full, "expected fullCollection=true")
			assert.Empty(t, ids, "fullCollection cases must not return ids")
		})
	}
}

func TestResolveInWithSubDocFallsToFullCollection(t *testing.T) {
	inner := must.NotFail(types.NewDocument("nested", "field"))
	in := must.NotFail(types.NewArray(int32(1), inner))
	filter := must.NotFail(types.NewDocument(
		"_id", must.NotFail(types.NewDocument("$in", in)),
	))

	ids, full, err := ResolveFilterToDocIDHashes(filter)
	require.NoError(t, err)
	assert.True(t, full)
	assert.Empty(t, ids)
}
