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
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/index"
)

func TestErrorBudgetShorten(t *testing.T) {
	big := strings.Repeat("\u00e9", 1<<20)
	medium := strings.Repeat("m", 1000000)
	small := strings.Repeat("s", 1000)

	var b errorBudget
	var got []string
	for _, msg := range []string{big, medium, big, medium, big, big} {
		got = append(got, b.shorten(msg))
	}
	require.Equal(t, []string{big, medium, big, medium, "", ""}, got)

	b = errorBudget{}
	total := 0
	for range 100000 {
		total += len(b.shorten(small))
	}
	require.LessOrEqual(t, total, errorMessagesBudget+len(small))
}

func TestErrorBudgetAppendError(t *testing.T) {
	big := strings.Repeat("b", 1<<20+1)
	var b errorBudget
	var errs []*mongo.WriteError
	for i := range 4 {
		errs = b.appendError(errs, &mongo.WriteError{Index: i, Code: 16755, Message: big})
	}
	var got []string
	for _, we := range errs {
		got = append(got, we.Message)
	}
	require.Equal(t, []string{big, big, "", ""}, got)
}

func TestErrorBudgetContext(t *testing.T) {
	big := strings.Repeat("b", 1<<20+1)
	var b errorBudget
	var marked []bool
	for range 3 {
		marked = append(marked, index.LargeMessagesEmptied(b.context(context.Background())))
		b.shorten(big)
	}
	require.Equal(t, []bool{false, false, true}, marked)

	b = errorBudget{total: errorMessagesBudget}
	require.True(t, index.LargeMessagesEmptied(b.context(context.Background())))
}

func TestIndexKeyErrorElided(t *testing.T) {
	err := fmt.Errorf("updating secondary indexes: %w", fmt.Errorf("%w: %w", index.ErrCannotExtractGeoKeys, index.ErrMessageElided))
	code, msg, ok := handlererrors.IndexKeyError(err)
	require.True(t, ok)
	require.Equal(t, handlererrors.ErrCannotExtractGeoKeys, code)
	require.Empty(t, msg)
}
