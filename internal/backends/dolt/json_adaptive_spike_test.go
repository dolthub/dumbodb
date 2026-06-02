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

// Spike test for workspace-9r3: verify val.JsonAdaptiveEnc supports
// the canonical dumbo document shape and inlines documents of typical
// size into the row tuple, bypassing the SerializeJsonToAddr chunker
// that writeDocJSON currently uses. See
// docs/design/document-storage-parity-with-dolt.md.

package dolt

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/pool"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/require"
)

// countingValueStore implements val.ValueStore and counts WriteBytes
// calls. The spike asserts WriteBytes is never invoked while inserting
// documents of typical Mongo size -- a non-zero count would mean the
// tuple builder spilled the document out-of-band, which is the path
// we are trying to avoid.
type countingValueStore struct {
	chunks map[hash.Hash][]byte
	writes atomic.Int64
}

func (s *countingValueStore) ReadBytes(_ context.Context, h hash.Hash) ([]byte, error) {
	return s.chunks[h], nil
}

func (s *countingValueStore) WriteBytes(_ context.Context, v []byte) (hash.Hash, error) {
	s.writes.Add(1)
	h := hash.Of(v)
	if s.chunks == nil {
		s.chunks = make(map[hash.Hash][]byte)
	}
	s.chunks[h] = append([]byte(nil), v...)
	return h, nil
}

// TestSpikeJsonAdaptiveEnc_InlineRoundTrip writes ten canonical
// dumbo-shape documents through the JsonAdaptiveEnc tuple-builder
// path, reads each one back, and asserts (a) round-trip equality
// and (b) zero writes to the value store. The second assertion is
// the load-bearing one: writeDocJSON's current path produces one
// out-of-band JSON chunk per document (one ValueStore.WriteBytes
// call); the JsonAdaptiveEnc inline path produces none.
func TestSpikeJsonAdaptiveEnc_InlineRoundTrip(t *testing.T) {
	ctx := sql.NewEmptyContext()
	vs := &countingValueStore{}

	td := val.NewTupleDescriptor(val.Type{Enc: val.JsonAdaptiveEnc})
	p := pool.NewBuffPool()

	const n = 10
	for i := 0; i < n; i++ {
		doc := canonicalDocJSON(i)

		tb := val.NewTupleBuilder(td, vs)
		require.NoError(t, tb.PutAdaptiveJsonFromInline(ctx, 0, doc))

		tup, err := tb.Build(ctx, p)
		require.NoError(t, err)

		result, ok, err := td.GetJsonAdaptiveValue(ctx, 0, vs, tup)
		require.NoError(t, err)
		require.True(t, ok)

		gotBytes, ok := result.([]byte)
		require.Truef(t, ok, "doc %d: expected inline []byte, got %T", i, result)
		require.JSONEq(t, string(doc), string(gotBytes))
	}

	require.Equalf(t, int64(0), vs.writes.Load(),
		"JsonAdaptiveEnc inline writes must not call ValueStore.WriteBytes "+
			"-- canonical dumbo documents of ~%d bytes are kept in the tuple",
		len(canonicalDocJSON(0)))
}

// canonicalDocJSON produces a canonical {_id, email, name, age}
// Extended-JSON document of roughly 100 bytes, mirroring what
// writeDocJSON emits after the BSON -> ExtJSON conversion.
func canonicalDocJSON(i int) []byte {
	m := map[string]any{
		"_id":   fmt.Sprintf("doc%07d", i),
		"email": fmt.Sprintf("user%07d@example.com", i),
		"name":  fmt.Sprintf("User %d", i),
		"age":   map[string]any{"$numberInt": fmt.Sprintf("%d", 20+i)},
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}
