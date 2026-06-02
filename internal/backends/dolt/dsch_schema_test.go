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

// Schema-shape assertion for workspace-r11: confirms new collections
// declare the document column with EncodingJsonAdaptive (was
// EncodingJSONAddr before this task) and the key column with
// EncodingBytes. The encoding is what tells Dolt how to lay out the
// stored value, so a regression here puts dumbo back on the per-doc
// chunker path that the storage-parity investigation flagged.

package dolt

import (
	"testing"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/stretchr/testify/require"
)

func TestCollectionTableSchema_UsesJsonAdaptiveEnc(t *testing.T) {
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
	require.Equalf(t, serial.EncodingJsonAdaptive, c1.Encoding(),
		"doc column encoding must be JsonAdaptive (was %v); JSONAddr puts dumbo back on "+
			"the per-doc chunker path -- see docs/design/document-storage-parity-with-dolt.md",
		c1.Encoding())
}
