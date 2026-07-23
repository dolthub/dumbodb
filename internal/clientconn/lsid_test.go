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

package clientconn

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
)

func makeCtx() (context.Context, *conninfo.ConnInfo) {
	ci := conninfo.New()
	return conninfo.Ctx(context.Background(), ci), ci
}

func docWithLSID(uuidBytes []byte) *types.Document {
	lsidSub := types.MakeDocument(1)
	lsidSub.Set("id", types.Binary{B: uuidBytes, Subtype: types.BinaryUUID})
	cmd := types.MakeDocument(2)
	cmd.Set("insert", "col")
	cmd.Set("lsid", lsidSub)
	return cmd
}

func TestExtractAndSetLSIDWithUUID(t *testing.T) {
	ctx, ci := makeCtx()
	uuid := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}
	extractAndSetLSID(ctx, docWithLSID(uuid))
	assert.Equal(t, "0102030405060708090a0b0c0d0e0f10", ci.LSID())
}

func TestExtractAndSetLSIDNoLSIDIsNoop(t *testing.T) {
	ctx, ci := makeCtx()
	cmd := types.MakeDocument(1)
	cmd.Set("ping", int32(1))
	extractAndSetLSID(ctx, cmd)
	assert.Equal(t, "", ci.LSID())

	assert.Contains(t, ci.Owner(), "conn:")
}

func TestExtractAndSetLSIDMalformedIsNoop(t *testing.T) {
	cases := []struct {
		name string
		doc  *types.Document
	}{
		{
			name: "lsid is not a document",
			doc: func() *types.Document {
				d := types.MakeDocument(1)
				d.Set("lsid", "not a document")
				return d
			}(),
		},
		{
			name: "lsid has no id field",
			doc: func() *types.Document {
				inner := types.MakeDocument(0)
				d := types.MakeDocument(1)
				d.Set("lsid", inner)
				return d
			}(),
		},
		{
			name: "lsid.id is not Binary",
			doc: func() *types.Document {
				inner := types.MakeDocument(1)
				inner.Set("id", "string-not-binary")
				d := types.MakeDocument(1)
				d.Set("lsid", inner)
				return d
			}(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, ci := makeCtx()
			extractAndSetLSID(ctx, c.doc)
			assert.Equal(t, "", ci.LSID(), "malformed lsid must not corrupt ConnInfo")
		})
	}
}

func TestExtractAndSetLSIDNilDocIsNoop(t *testing.T) {
	ctx, ci := makeCtx()
	extractAndSetLSID(ctx, nil)
	assert.Equal(t, "", ci.LSID())
}
