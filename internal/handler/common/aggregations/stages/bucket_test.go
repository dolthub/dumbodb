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

package stages_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestBucketLargeBoundariesNotFolded: once the $switch is too large, the
// remaining boundaries are not folded, so many large ones and a large default
// are refused in about the time of folding three of them.
func TestBucketLargeBoundariesNotFolded(t *testing.T) {
	boundaries := types.MakeArray(1000)
	for i := range 1000 {
		boundaries.Append(must.NotFail(types.NewDocument("$range", must.NotFail(types.NewArray(int32(i), int32(i+3000000))))))
	}
	stage := must.NotFail(types.NewDocument("$bucket", must.NotFail(types.NewDocument(
		"groupBy", "$x",
		"boundaries", boundaries,
		"default", must.NotFail(types.NewDocument("$range", must.NotFail(types.NewArray(int32(-3000000), int32(0))))),
	))))

	_, err := stages.NewStage(stage)
	var cerr *handlererrors.CommandError
	require.True(t, errors.As(err, &cerr), "%v", err)
	require.Equal(t, handlererrors.ErrBSONObjectTooLarge, cerr.Code())
}
