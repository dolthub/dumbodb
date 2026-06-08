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
	"testing"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/stretchr/testify/require"
)

func TestCollectionTableSchema_UsesBytesAdaptiveEnc(t *testing.T) {
	msg := buildCollectionTableSchema()

	ts, err := serial.TryGetRootAsTableSchema(msg, serial.MessagePrefixSz)
	require.NoError(t, err)

	require.Equal(t, 2, ts.ColumnsLength(), "DSCH must have exactly two columns: _id and doc")

	var c0, c1 serial.Column
	ok, err := ts.TryColumns(&c0, 0)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "_id", string(c0.Name()))
	require.Equal(t, serial.EncodingBytes, c0.Encoding(), "_id encoding must be Bytes")

	ok, err = ts.TryColumns(&c1, 1)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "doc", string(c1.Name()))
	require.Equalf(t, serial.EncodingBytesAdaptive, c1.Encoding(),
		"doc column encoding must be BytesAdaptive (got %v); see "+
			"docs/design/bson-type-fidelity-and-storage-overhead.md",
		c1.Encoding())
}
