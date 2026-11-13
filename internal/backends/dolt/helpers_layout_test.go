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

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/hash"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	fb "github.com/dolthub/flatbuffers/v23/go"
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

// A table whose schema field is missing or not a hash is refused, not a panic.
func TestCheckTableLayoutRefusesMalformedSchema(t *testing.T) {
	for _, schema := range [][]byte{nil, make([]byte, 5)} {
		b := fb.NewBuilder(64)
		var schOff fb.UOffsetT
		if schema != nil {
			schOff = b.CreateByteVector(schema)
		}
		serial.TableStart(b)
		if schema != nil {
			serial.TableAddSchema(b, schOff)
		}
		msg := serial.FinishMessage(b, serial.TableEnd(b), []byte(serial.TableFileID))
		tbl, err := serial.TryGetRootAsTable(msg, serial.MessagePrefixSz)
		require.NoError(t, err)
		require.ErrorIs(t, checkTableLayout(tbl), errUnsupportedLayout, "schema of %d bytes", len(schema))
	}
}

// TestStoredHashRefusesWrongLength: a stored hash field of another length is
// an error, where hash.New panics.
func TestStoredHashRefusesWrongLength(t *testing.T) {
	for _, n := range []int{0, 1, hash.ByteLen - 1, hash.ByteLen + 1} {
		if _, err := storedHash(make([]byte, n), "artifacts hash"); err == nil {
			t.Errorf("%d bytes: no error", n)
		}
	}
	want := hash.Of([]byte("x"))
	if got, err := storedHash(want[:], "artifacts hash"); err != nil || got != want {
		t.Errorf("20 bytes: got %v, %v", got, err)
	}
}
