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
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/util/must"
)

// Tables reference this version's collection schema; one referencing another
// schema, as an older version's hash-keyed tables do, is refused.
func TestOpenCollectionRefusesOtherSchema(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "layoutdb", "coll", mustDoc(t, "_id", int32(1)))
	state := mustDB(t, b, "layoutdb")
	require.Equal(t, collectionSchemaHash, state.collSchemaHash)

	primary := must.NotFail(newEmptyMap(ctx, state.ns))
	indexes := must.NotFail(newSecondaryIndexes(ctx, state.ns))
	old := buildDoltTableFlatbuffer(primary, hash.Of([]byte("binary(20) schema")), indexes.tableBytes(), hash.Hash{})
	ref, err := state.vs.WriteValue(ctx, dolttypes.SerialMessage(old))
	require.NoError(t, err)

	_, err = openCollection(ctx, state.cs, state.ns, ref.TargetHash())
	require.ErrorIs(t, err, errUnsupportedLayout)
	_, err = secondaryIndexesForDTBL(ctx, state.cs, state.ns, ref.TargetHash())
	require.ErrorIs(t, err, errUnsupportedLayout)
}
