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

package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

func TestRejectUnknownFields(t *testing.T) {
	t.Run("CommandKeyAndAllowedAccepted", func(t *testing.T) {
		doc := mustDoc(types.NewDocument("dumboMerge", int32(1), "mergeIn", "feature", "noFF", true))
		require.NoError(t, RejectUnknownFields(doc, "mergeIn", "noFF", "ffOnly"))
	})

	t.Run("FullDriverEnvelopeAccepted", func(t *testing.T) {
		doc := mustDoc(types.NewDocument(
			"ping", int32(1),
			"$db", "admin",
			"lsid", mustDoc(types.NewDocument("id", "x")),
			"$clusterTime", mustDoc(types.NewDocument("t", int32(1))),
			"$readPreference", mustDoc(types.NewDocument("mode", "primary")),
			"comment", "hi",
			"maxTimeMS", int32(1000),
			"apiVersion", "1",
		))
		require.NoError(t, RejectUnknownFields(doc))
	})

	t.Run("UnknownFieldRejected", func(t *testing.T) {
		doc := mustDoc(types.NewDocument("dumboMerge", int32(1), "mergeIn", "feature", "noff", true))
		err := RejectUnknownFields(doc, "mergeIn", "noFF", "ffOnly")
		require.Error(t, err)

		var ce *handlererrors.CommandError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, handlererrors.ErrIDLUnknownField, ce.Code())
		assert.Contains(t, err.Error(), "BSON field 'dumboMerge.noff' is an unknown field.")
	})

	t.Run("FirstUnknownReported", func(t *testing.T) {
		doc := mustDoc(types.NewDocument("count", "c", "bogusA", int32(1), "bogusB", int32(2)))
		err := RejectUnknownFields(doc)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "count.bogusA")
	})
}

func mustDoc(d *types.Document, err error) *types.Document {
	if err != nil {
		panic(err)
	}
	return d
}
