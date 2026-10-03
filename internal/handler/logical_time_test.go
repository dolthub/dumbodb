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
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
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

func TestAuthenticatedExternalLogicalTimeUsesReplicatedKey(t *testing.T) {
	h := authGateHandler(t, true)
	h.MembershipCredentials = must.NotFail(membership.New("abcdef"))
	database := must.NotFail(h.b.Database("admin"))
	collection := must.NotFail(database.Collection("system.keys"))
	keyBytes := make([]byte, 20)
	for index := range keyBytes {
		keyBytes[index] = byte(index)
	}
	keyID := int64(7692235776586678278)
	expiresAt := types.NewTimestamp(time.Now().Add(time.Hour).UTC(), 1)
	key := must.NotFail(types.NewDocument(
		"_id", keyID,
		"purpose", "HMAC",
		"key", types.Binary{Subtype: types.BinaryGeneric, B: keyBytes},
		"expiresAt", expiresAt,
	))
	if _, err := collection.InsertAll(context.Background(), &backends.InsertAllParams{Docs: []*types.Document{key}}); err != nil {
		t.Fatal(err)
	}

	info := conninfo.New()
	info.SetAuth("root", "", nil, "admin")
	info.SetAuthenticated()
	ctx := conninfo.Ctx(context.Background(), info)
	response, err := h.withLogicalTime(ctx, wire.MustOpMsg("ok", float64(1)))
	if err != nil {
		t.Fatal(err)
	}
	document := must.NotFail(opMsgDocument(response))
	clusterTime := must.NotFail(document.Get("$clusterTime")).(*types.Document)
	timestamp := must.NotFail(clusterTime.Get("clusterTime")).(types.Timestamp)
	signature := must.NotFail(clusterTime.Get("signature")).(*types.Document)
	if got := must.NotFail(signature.Get("keyId")); got != keyID {
		t.Fatalf("keyId = %v, want %d", got, keyID)
	}
	hash := must.NotFail(signature.Get("hash")).(types.Binary)
	want := signLogicalTime(timestamp, keyBytes)
	if !bytes.Equal(hash.B, want) {
		t.Fatalf("signature = %x, want %x", hash.B, want)
	}
}

func TestSignLogicalTimeMatchesMongoDBTimeProof(t *testing.T) {
	key := make([]byte, 20)
	for index := range key {
		key[index] = byte(index)
	}
	logicalTime := types.Timestamp(uint64(0x01020304)<<32 | 0x05060708)
	want := must.NotFail(hex.DecodeString("571773887c9fb972c000feb1a444a8d8f235907c"))
	if got := signLogicalTime(logicalTime, key); !bytes.Equal(got, want) {
		t.Fatalf("signature = %x, want %x", got, want)
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
