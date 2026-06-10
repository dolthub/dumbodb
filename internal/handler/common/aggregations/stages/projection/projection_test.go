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

package projection

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestProjectDocument_NoIDInput pins that a $project over a document
// that lacks _id does not panic and yields output without _id, matching
// MongoDB. Pipeline stages upstream of $project ($project {_id:0},
// $group, $unwind of a projected stream) routinely emit such documents;
// the previous implementation seeded the result with
// must.NotFail(doc.Get("_id")) and crashed the connection (the panic
// that broke MongoDB Compass).
func TestProjectDocument_NoIDInput(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		doc        *types.Document
		projection *types.Document
		wantHasID  bool
	}{
		"inclusion, no _id in input": {
			doc:        must.NotFail(types.NewDocument("tags", "red", "n", int32(1))),
			projection: must.NotFail(types.NewDocument("tags", int32(1))),
			wantHasID:  false,
		},
		"exclusion, no _id in input": {
			doc:        must.NotFail(types.NewDocument("tags", "red", "n", int32(1))),
			projection: must.NotFail(types.NewDocument("n", int32(0))),
			wantHasID:  false,
		},
		"inclusion, _id present passes through": {
			doc:        must.NotFail(types.NewDocument("_id", int32(7), "tags", "red")),
			projection: must.NotFail(types.NewDocument("tags", int32(1))),
			wantHasID:  true,
		},
		"explicit _id literal, absent in input, still set": {
			doc:        must.NotFail(types.NewDocument("tags", "red")),
			projection: must.NotFail(types.NewDocument("_id", "computed", "tags", int32(1))),
			wantHasID:  true,
		},
		"explicit _id:0, absent in input, stays absent": {
			doc:        must.NotFail(types.NewDocument("tags", "red")),
			projection: must.NotFail(types.NewDocument("_id", int32(0), "tags", int32(1))),
			wantHasID:  false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			validated, inclusion, err := ValidateProjection(tc.projection)
			require.NoError(t, err)

			var projected *types.Document
			require.NotPanics(t, func() {
				projected, err = ProjectDocument(tc.doc, validated, inclusion)
			}, "ProjectDocument must not panic on an input without _id")
			require.NoError(t, err)

			assert.Equal(t, tc.wantHasID, projected.Has("_id"),
				"projected _id presence; got doc %v", projected)
		})
	}
}
