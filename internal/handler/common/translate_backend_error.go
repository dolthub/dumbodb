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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
)

func TranslateBackendWriteError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if backends.ErrorCodeIs(err, backends.ErrorCodeReadOnlyDatabase) {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"cannot write to a read-only database snapshot",
		)
	}
	if backends.ErrorCodeIs(err, backends.ErrorCodeReadOnlyCollection) {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrIllegalOperation,
			"cannot modify a protected system collection",
		)
	}
	if backends.ErrorCodeIs(err, backends.ErrorCodeWriteConflict) {
		if ci := conninfo.GetIfPresent(ctx); ci != nil && ci.InTransaction() {
			return handlererrors.NewCommandErrorWithLabels(
				handlererrors.ErrWriteConflict,
				err,
				handlererrors.TransientTransactionErrorLabel,
			)
		}
		return handlererrors.NewCommandError(handlererrors.ErrWriteConflict, err)
	}
	return err
}
