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
	"testing"

	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/sqlctx"
)

type commitErrorBackend struct {
	abortedOwner string
}

func (*commitErrorBackend) OnSessionEnd(string) {}

func (*commitErrorBackend) OnTransactionCommit(context.Context, string) error { return nil }

func (b *commitErrorBackend) OnTransactionAbort(owner string) { b.abortedOwner = owner }

func (*commitErrorBackend) SessionIsolation() bool { return true }

func (*commitErrorBackend) SessionRegistry() *sqlctx.SessionRegistry { return nil }

func TestHandleTransactionCommitConflict(t *testing.T) {
	for _, commitErr := range []error{
		backends.ErrWriteRaced,
		&backends.MergeConflictError{},
	} {
		t.Run(commitErr.Error(), func(t *testing.T) {
			backend := &commitErrorBackend{}
			info := conninfo.New()
			info.SetInTransaction(true)

			err := handleTransactionCommitError(backend, info, commitErr)
			var commandErr *handlererrors.CommandError
			if !errors.As(err, &commandErr) {
				t.Fatalf("error has type %T", err)
			}
			if commandErr.Code() != handlererrors.ErrWriteConflict {
				t.Fatalf("code=%v", commandErr.Code())
			}
			labels, ok := commandErr.Document().Get("errorLabels").(*wirebson.Array)
			if !ok || labels.Len() != 1 || labels.Get(0) != handlererrors.TransientTransactionErrorLabel {
				t.Fatalf("labels=%v", labels)
			}
			if info.InTransaction() {
				t.Fatal("connection remains in a transaction")
			}
			if backend.abortedOwner != info.Owner() {
				t.Fatalf("aborted owner=%q want=%q", backend.abortedOwner, info.Owner())
			}
		})
	}
}

func TestHandleTransactionCommitUnrelatedError(t *testing.T) {
	backend := &commitErrorBackend{}
	info := conninfo.New()
	info.SetInTransaction(true)
	want := errors.New("unrelated")

	got := handleTransactionCommitError(backend, info, want)
	if !errors.Is(got, want) {
		t.Fatalf("error=%v", got)
	}
	if !info.InTransaction() {
		t.Fatal("unrelated error ended the transaction")
	}
	if backend.abortedOwner != "" {
		t.Fatalf("unexpected abort for %q", backend.abortedOwner)
	}
}
