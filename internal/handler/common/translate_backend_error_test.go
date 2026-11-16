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

package common

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
)

// mongod refuses a create in a database that is being dropped with code 215.
func TestTranslateBackendWriteErrorDatabaseDropPending(t *testing.T) {
	backendErr := backends.NewError(backends.ErrorCodeDatabaseDropPending,
		errors.New(`database "victim" is in the process of being dropped`))

	err := TranslateBackendWriteError(context.Background(), fmt.Errorf("inserting: %w", backendErr))

	var commandErr *handlererrors.CommandError
	require.ErrorAs(t, err, &commandErr)
	require.Equal(t, handlererrors.ErrDatabaseDropPending, commandErr.Code())
	require.Equal(t, handlererrors.ErrorCode(215), commandErr.Code())
	require.Contains(t, err.Error(), "in the process of being dropped")
}
