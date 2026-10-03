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
	"testing"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/replication/membership"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestWithLogicalTime(t *testing.T) {
	h := &Handler{}
	response := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1)))))
	response.Flags = wire.OpMsgFlags(wire.OpMsgMoreToCome)

	first, err := h.withLogicalTime(context.Background(), response)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.withLogicalTime(context.Background(), response)
	if err != nil {
		t.Fatal(err)
	}

	firstTime := responseLogicalTime(t, first)
	secondTime := responseLogicalTime(t, second)
	if secondTime <= firstTime {
		t.Fatalf("logical time did not advance: first=%v second=%v", firstTime, secondTime)
	}
	if !first.Flags.FlagSet(wire.OpMsgMoreToCome) || !second.Flags.FlagSet(wire.OpMsgMoreToCome) {
		t.Fatal("logical-time post-processing dropped moreToCome")
	}
}

func TestInternalLogicalTimeRequiresMembershipAuthentication(t *testing.T) {
	h := &Handler{NewOpts: &NewOpts{}}
	logicalTime := types.NewTimestamp(time.Now().UTC(), 7)
	request := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"ping", int32(1),
		"$clusterTime", must.NotFail(types.NewDocument("clusterTime", logicalTime)),
		"$db", "admin",
	))))
	info := conninfo.New()
	ctx := conninfo.Ctx(context.Background(), info)
	if err := h.observeLogicalTime(ctx, request); err == nil {
		t.Fatal("observeLogicalTime accepted unsigned time before membership authentication")
	}
	info.SetAuth(membership.Username, "", nil, membership.Database)
	info.SetAuthenticated()
	if err := h.observeLogicalTime(ctx, request); err != nil {
		t.Fatal(err)
	}
	if h.logicalTime.Load() != uint64(logicalTime) {
		t.Fatalf("logical time = %d, want %d", h.logicalTime.Load(), logicalTime)
	}

	response, err := h.withLogicalTime(ctx, wire.MustOpMsg("ok", float64(1)))
	if err != nil {
		t.Fatal(err)
	}
	document, err := opMsgDocument(response)
	if err != nil {
		t.Fatal(err)
	}
	clusterTime := must.NotFail(document.Get("$clusterTime")).(*types.Document)
	if clusterTime.Has("signature") {
		t.Fatal("internal $clusterTime response contains a signature")
	}
}

func responseLogicalTime(t *testing.T, response *wire.OpMsg) types.Timestamp {
	t.Helper()
	doc, err := opMsgDocument(response)
	if err != nil {
		t.Fatal(err)
	}
	operationTime, err := doc.Get("operationTime")
	if err != nil {
		t.Fatal(err)
	}
	timestamp, ok := operationTime.(types.Timestamp)
	if !ok {
		t.Fatalf("operationTime has type %T", operationTime)
	}
	clusterTimeValue, err := doc.Get("$clusterTime")
	if err != nil {
		t.Fatal(err)
	}
	clusterTime, ok := clusterTimeValue.(*types.Document)
	if !ok {
		t.Fatalf("$clusterTime has type %T", clusterTimeValue)
	}
	clusterTimestamp, err := clusterTime.Get("clusterTime")
	if err != nil {
		t.Fatal(err)
	}
	if clusterTimestamp != timestamp {
		t.Fatalf("clusterTime=%v operationTime=%v", clusterTimestamp, timestamp)
	}
	return timestamp
}
