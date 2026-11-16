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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCapErrorMessages(t *testing.T) {
	big := strings.Repeat("\u00e9", 1<<20)
	medium := strings.Repeat("m", 1000000)
	small := strings.Repeat("s", 1000)

	errs := types.MakeArray(0)
	for _, msg := range []string{big, medium, big, medium, big, big} {
		errs.Append(must.NotFail(types.NewDocument("index", int32(0), "code", int32(16755), "errmsg", msg)))
	}
	errs.Append(must.NotFail(types.NewDocument("index", int32(0), "ok", int32(1))))
	capErrorMessages(errs)
	require.Equal(t, []string{big, medium, big, medium, "", ""}, errorMessages(errs))

	errs = types.MakeArray(0)
	for range 100000 {
		errs.Append(must.NotFail(types.NewDocument("index", int32(0), "code", int32(16755), "errmsg", small)))
	}
	capErrorMessages(errs)
	total := 0
	for _, msg := range errorMessages(errs) {
		total += len(msg)
	}
	require.LessOrEqual(t, total, errorMessagesBudget+len(small))
}

func errorMessages(errs *types.Array) []string {
	var out []string
	for i := range errs.Len() {
		doc := must.NotFail(errs.Get(i)).(*types.Document)
		if v, err := doc.Get("errmsg"); err == nil {
			out = append(out, v.(string))
		}
	}
	return out
}
