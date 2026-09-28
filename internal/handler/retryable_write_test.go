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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestRejectRetryableWriteOnStandalone(t *testing.T) {
	cases := []struct {
		name       string
		doc        *types.Document
		replSet    string
		wantReject bool
	}{
		{
			name:       "retryable write on standalone is rejected",
			doc:        must.NotFail(types.NewDocument("update", "col", "txnNumber", int64(1))),
			replSet:    "",
			wantReject: true,
		},
		{
			name:       "retryable write with txnNumber 0 is still rejected (presence, not value)",
			doc:        must.NotFail(types.NewDocument("update", "col", "txnNumber", int64(0))),
			replSet:    "",
			wantReject: true,
		},
		{
			name:       "transaction (autocommit present) is allowed on standalone",
			doc:        must.NotFail(types.NewDocument("update", "col", "txnNumber", int64(1), "autocommit", false)),
			replSet:    "",
			wantReject: false,
		},
		{
			name:       "plain write without txnNumber is allowed",
			doc:        must.NotFail(types.NewDocument("update", "col")),
			replSet:    "",
			wantReject: false,
		},
		{
			name:       "retryable write on a replica set member is allowed",
			doc:        must.NotFail(types.NewDocument("update", "col", "txnNumber", int64(1))),
			replSet:    "rs0",
			wantReject: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectRetryableWriteOnStandalone(tc.doc, tc.replSet)
			if !tc.wantReject {
				if err != nil {
					t.Fatalf("want no rejection, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want IllegalOperation rejection, got nil")
			}
			var ce *handlererrors.CommandError
			if !errors.As(err, &ce) {
				t.Fatalf("want *CommandError, got %T: %v", err, err)
			}
			if ce.Code() != handlererrors.ErrIllegalOperation {
				t.Fatalf("want code %d (IllegalOperation), got %d", handlererrors.ErrIllegalOperation, ce.Code())
			}
		})
	}
}
