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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCommitDateParam(t *testing.T) {
	bsonDate := time.Date(2020, 6, 15, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		value any
		want  time.Time
		code  handlererrors.ErrorCode
	}{
		{name: "missing"},
		{name: "bson date", value: bsonDate, want: bsonDate},
		{name: "bson date year 1", value: time.Time{}, want: time.Time{}},
		{name: "string year 1", value: "0001-01-01", want: time.Time{}},
		{name: "rfc3339 utc", value: "2024-01-02T15:04:05Z", want: time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC)},
		{name: "rfc3339 fractional", value: "2024-01-02T15:04:05.123Z", want: time.Date(2024, 1, 2, 15, 4, 5, 123e6, time.UTC)},
		{name: "rfc3339 offset", value: "2024-01-02T10:04:05-05:00", want: time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC)},
		{name: "bare date", value: "2024-01-02", want: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
		{name: "unparseable string", value: "yesterday", code: handlererrors.ErrBadValue},
		{name: "zone-less time", value: "2024-01-02T15:04:05", code: handlererrors.ErrBadValue},
		{name: "wrong type", value: int32(5), code: handlererrors.ErrTypeMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := must.NotFail(types.NewDocument())
			if tc.value != nil {
				doc.Set("timestamp", tc.value)
			}

			got, err := commitDateParam(doc, "timestamp")
			if tc.code != 0 {
				var ce *handlererrors.CommandError
				require.True(t, errors.As(err, &ce), "want a CommandError, got %v", err)
				require.Equal(t, tc.code, ce.Code())
				return
			}

			require.NoError(t, err)
			if tc.value == nil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.True(t, tc.want.Equal(*got), "want %v, got %v", tc.want, *got)
		})
	}
}
