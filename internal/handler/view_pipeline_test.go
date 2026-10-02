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

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestPipelineForeignNamespaces(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }

	pipeline := arr(
		doc("$match", doc("x", int32(1))),
		doc("$lookup", doc("from", "a", "localField", "x", "foreignField", "y", "as", "out")),
		doc("$lookup", doc("from", "b", "pipeline", arr(doc("$lookup", doc("from", "c", "pipeline", arr(), "as", "inner"))), "as", "out")),
		doc("$graphLookup", doc("from", "d", "startWith", "$x", "connectFromField", "x", "connectToField", "y", "as", "out")),
		doc("$unionWith", "e"),
		doc("$unionWith", doc("coll", "f", "pipeline", arr(doc("$unionWith", "g")))),
		doc("$facet", doc("left", arr(doc("$lookup", doc("from", "h", "localField", "x", "foreignField", "y", "as", "out"))))),
	)

	require.ElementsMatch(t, []string{"a", "b", "c", "d", "e", "f", "g", "h"}, pipelineForeignNamespaces(pipeline))
	require.Empty(t, pipelineForeignNamespaces(nil))
}
