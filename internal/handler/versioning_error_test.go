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
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
)

// A version-control command's lost race and its refusal over uncommitted
// changes each reach the client with a code of their own.
func TestVersioningErrorCodes(t *testing.T) {
	inTxn := conninfo.New()
	inTxn.SetInTransaction(true)
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		err   error
		code  handlererrors.ErrorCode
		label bool
	}{
		{"lost race", conninfo.Ctx(context.Background(), conninfo.New()), fmt.Errorf("x: %w", backends.ErrWriteRaced), handlererrors.ErrWriteConflict, false},
		{"lost race in a transaction", conninfo.Ctx(context.Background(), inTxn), fmt.Errorf("x: %w", backends.ErrWriteRaced), handlererrors.ErrWriteConflict, true},
		{"uncommitted changes", conninfo.Ctx(context.Background(), conninfo.New()), fmt.Errorf("x: %w", backends.ErrUncommittedChanges), handlererrors.ErrConflictingOperationInProgress, false},
		{"an operation paused on conflicts", conninfo.Ctx(context.Background(), conninfo.New()), refusal{}, handlererrors.ErrConflictResolutionRequired, false},
		{"anything else", conninfo.Ctx(context.Background(), conninfo.New()), errors.New("boom"), handlererrors.ErrOperationFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ce *handlererrors.CommandError
			require.ErrorAs(t, versioningError(tc.ctx, tc.err), &ce)
			require.Equal(t, tc.code, ce.Code())
			require.Equal(t, tc.label, ce.Document().Get("errorLabels") != nil)
		})
	}
}

type refusal struct{}

func (refusal) Error() string        { return "merge in progress" }
func (refusal) Is(target error) bool { return target == backends.ErrOperationPaused }

// Once dumboCommit has committed the client's transaction, a lost race it then
// gives up on is not labelled TransientTransactionError: the driver would run
// the transaction again and apply writes that already committed.
func TestDumboCommitErrorAfterCommittingTheTransaction(t *testing.T) {
	inTxn := conninfo.New()
	inTxn.SetInTransaction(true)
	ctx := conninfo.Ctx(context.Background(), inTxn)
	raced := fmt.Errorf("x: %w", backends.ErrWriteRaced)
	for _, tc := range []struct {
		committedTxn bool
		label        bool
	}{{committedTxn: true, label: false}, {committedTxn: false, label: true}} {
		var ce *handlererrors.CommandError
		require.ErrorAs(t, dumboCommitError(ctx, raced, tc.committedTxn), &ce)
		require.Equal(t, handlererrors.ErrWriteConflict, ce.Code())
		require.Equal(t, tc.label, ce.Document().Get("errorLabels") != nil, "committedTxn=%v", tc.committedTxn)
	}
}
