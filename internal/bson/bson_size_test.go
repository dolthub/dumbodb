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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestBSONSizeMatchesEncoding(t *testing.T) {
	nested := must.NotFail(types.NewDocument("a", int32(1), "b", must.NotFail(types.NewArray("x", types.Null, int64(2)))))
	elevenElements := types.MakeArray(11)
	for i := 0; i < 11; i++ {
		elevenElements.Append(int32(i))
	}

	for name, doc := range map[string]*types.Document{
		"empty": must.NotFail(types.NewDocument()),
		"every type": must.NotFail(types.NewDocument(
			"_id", types.ObjectID{1, 2, 3},
			"double", 1.5,
			"string", "hello",
			"empty string", "",
			"document", nested,
			"array", must.NotFail(types.NewArray(int32(1), "two", nested)),
			"binary", types.Binary{B: []byte{1, 2, 3}, Subtype: types.BinaryUser},
			"bool", true,
			"date", time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC),
			"null", types.Null,
			"regex", types.Regex{Pattern: "^a.*", Options: "i"},
			"int32", int32(7),
			"timestamp", types.Timestamp(42),
			"int64", int64(1)<<40,
			"decimal", types.Decimal128{L: 1, H: 2},
		)),
		"min and max keys":          must.NotFail(types.NewDocument("lo", types.MinKey, "hi", must.NotFail(types.NewArray(types.MaxKey)))),
		"multi-digit array indexes": must.NotFail(types.NewDocument("a", elevenElements)),
		"large string":              must.NotFail(types.NewDocument("pad", strings.Repeat("x", 600*1024))),
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := FromDocumentRaw(doc)
			require.NoError(t, err)
			require.Equal(t, len(raw), doc.BSONSize())
		})
	}
}
