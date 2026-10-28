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

package bson

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func lazyFixture(t *testing.T) (*types.Document, []byte) {
	t.Helper()
	doc := must.NotFail(types.NewDocument(
		"_id", int32(1),
		"a", "x",
		"sub", must.NotFail(types.NewDocument("b", must.NotFail(types.NewArray(int32(1), "y")))),
	))
	raw := doc.AppendBSON(nil)
	return types.NewLazyDocument(append([]byte(nil), raw...)), raw
}

func TestLazyDocumentEncodesStoredBytes(t *testing.T) {
	lazy, raw := lazyFixture(t)
	require.Equal(t, "_id", lazy.Command())
	require.Equal(t, len(raw), lazy.BSONSize())
	require.Equal(t, raw, lazy.AppendBSON(nil))

	require.Equal(t, 3, lazy.Len())
	require.Equal(t, "x", must.NotFail(lazy.Get("a")))
	require.Equal(t, raw, lazy.AppendBSON(nil), "reading must keep the stored bytes")
}

func TestLazyDocumentChangeReencodes(t *testing.T) {
	for name, change := range map[string]func(d *types.Document){
		"Set":          func(d *types.Document) { d.Set("a", "z") },
		"Remove":       func(d *types.Document) { d.Remove("a") },
		"SetByPath":    func(d *types.Document) { must.NoError(d.SetByPath(must.NotFail(types.NewPathFromString("sub.c")), int32(2))) },
		"RemoveByPath": func(d *types.Document) { d.RemoveByPath(must.NotFail(types.NewPathFromString("sub.b"))) },
		"Append":       func(d *types.Document) { d.AppendDecoded("z", int32(3)) },
	} {
		t.Run(name, func(t *testing.T) {
			lazy, _ := lazyFixture(t)
			want, _ := lazyFixture(t)
			change(lazy)
			change(want)
			require.Equal(t, want.Keys(), lazy.Keys())
			got := lazy.AppendBSON(nil)
			decoded, err := DecodeRawDocument(got)
			require.NoError(t, err)
			require.Equal(t, types.Equal, types.Compare(want, decoded))
		})
	}
}

func TestLazyDocumentDeepCopy(t *testing.T) {
	lazy, raw := lazyFixture(t)
	lazy.SetRecordID(7)
	copied := lazy.DeepCopy()
	require.Equal(t, int64(7), copied.RecordID())
	copied.Set("a", "changed")
	require.Equal(t, raw, lazy.AppendBSON(nil))
	require.Equal(t, "changed", must.NotFail(copied.Get("a")))
}

func TestLazyEmptyDocument(t *testing.T) {
	raw := types.MakeDocument(0).AppendBSON(nil)
	lazy := types.NewLazyDocument(raw)
	require.Equal(t, "", lazy.Command())
	require.Equal(t, 0, lazy.Len())
	require.Equal(t, raw, lazy.AppendBSON(nil))
}
