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
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCapErrorMessages(t *testing.T) {
	big := strings.Repeat("\u00e9", 1<<20)
	errs := types.MakeArray(0)
	for range 66 {
		errs.Append(must.NotFail(types.NewDocument("index", int32(0), "code", int32(16755), "errmsg", big)))
	}
	errs.Append(must.NotFail(types.NewDocument("index", int32(0), "ok", int32(1))))
	capErrorMessages(errs)

	total := 0
	for i := range errs.Len() {
		doc := must.NotFail(errs.Get(i)).(*types.Document)
		v, err := doc.Get("errmsg")
		if err != nil {
			continue
		}
		msg := v.(string)
		require.True(t, utf8.ValidString(msg))
		if i < 2 {
			require.Equal(t, big, msg)
		} else {
			require.LessOrEqual(t, len(msg), shortenedErrorMessage+len("..."))
		}
		total += len(msg)
	}
	require.Less(t, total, errorMessagesBudget+len(big)+66*(shortenedErrorMessage+3))
}
