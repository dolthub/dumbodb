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
	"io"
	"log/slog"
	"testing"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/testutil"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestApplyResponsePostProcessorPreservesSuccessfulResponseOnError(t *testing.T) {
	h := &Handler{NewOpts: &NewOpts{L: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	response := &wire.OpMsg{}

	got := h.applyResponsePostProcessor(context.Background(), "forced failure", response, func() (*wire.OpMsg, error) {
		return nil, errors.New("forced opMsgDocument failure")
	})
	if got != response {
		t.Fatal("post-processing replaced the successful response")
	}
}

func TestPostProcessLogicalTimeRequiresReplicaSetConfiguration(t *testing.T) {
	_, store := testutil.NewControlStore(t, control.Configuration{
		SetName: "rs0", MemberHost: "dumbo.example:27017",
	})
	unconfigured := topology.New(store)

	tests := []struct {
		name        string
		handler     *Handler
		wantLogical bool
	}{
		{
			name: "standalone",
			handler: &Handler{NewOpts: &NewOpts{
				L: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}},
		},
		{
			name: "set name without configuration",
			handler: &Handler{NewOpts: &NewOpts{
				L:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
				ReplicationTopology: unconfigured,
			}},
		},
		{
			name:        "configured replica set",
			handler:     configuredReplicationHandler(t),
			wantLogical: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
				"ping", int32(1), "$db", "admin",
			))))
			response := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1)))))
			processed := test.handler.postProcessResponse(context.Background(), request, response)
			document, err := opMsgDocument(processed)
			if err != nil {
				t.Fatal(err)
			}
			if document.Has("operationTime") != test.wantLogical {
				t.Fatalf("operationTime present = %t, want %t", document.Has("operationTime"), test.wantLogical)
			}
			if document.Has("$clusterTime") != test.wantLogical {
				t.Fatalf("$clusterTime present = %t, want %t", document.Has("$clusterTime"), test.wantLogical)
			}
		})
	}
}
