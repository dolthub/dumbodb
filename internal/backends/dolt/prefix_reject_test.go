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
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestScanPredicatesOnPrefix(t *testing.T) {
	doc := must.NotFail(types.NewDocument("_id", int32(1), "grp", int32(3), "payload", strings.Repeat("x", 100), "tag", "t"))
	stored := must.NotFail(docToBSON(doc))
	prefix := stored[:40]
	for _, tc := range []struct {
		name   string
		filter *types.Document
		reject bool
	}{
		{"equality mismatch before the cut", must.NotFail(types.NewDocument("grp", int32(4))), true},
		{"equality match before the cut", must.NotFail(types.NewDocument("grp", int32(3))), false},
		{"field after the cut", must.NotFail(types.NewDocument("tag", "u")), false},
		{"range mismatch before the cut", must.NotFail(types.NewDocument("grp", must.NotFail(types.NewDocument("$gt", int32(5))))), true},
		{"range match before the cut", must.NotFail(types.NewDocument("grp", must.NotFail(types.NewDocument("$lt", int32(5))))), false},
		{"range on a field after the cut", must.NotFail(types.NewDocument("zz", must.NotFail(types.NewDocument("$lt", int32(5))))), false},
		{"field cut mid-value", must.NotFail(types.NewDocument("payload", "y")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reject := buildScanPrefixRejecter(tc.filter)
			require.NotNil(t, reject)
			require.Equal(t, tc.reject, reject(prefix))
		})
	}
}

func TestPrefixRejectKeepsLargeDocumentMatches(t *testing.T) {
	big := strings.Repeat("p", 40000)
	var docs []*types.Document
	for i := int32(1); i <= 40; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "grp", i%4, "payload", big, "tag", "t")))
	}
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "other", Key: []backends.IndexKeyPair{{Field: "other"}}}, docs...)
	ids, _ := sortWalkQuery(t, c, &backends.QueryParams{Filter: must.NotFail(types.NewDocument("grp", int32(1)))})
	slices.Sort(ids)
	require.Equal(t, []int32{1, 5, 9, 13, 17, 21, 25, 29, 33, 37}, ids)
	ids, _ = sortWalkQuery(t, c, &backends.QueryParams{Filter: must.NotFail(types.NewDocument("tag", "t", "grp", int32(2)))})
	slices.Sort(ids)
	require.Equal(t, []int32{2, 6, 10, 14, 18, 22, 26, 30, 34, 38}, ids)
}
