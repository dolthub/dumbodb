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

package clientconn

import (
	"testing"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
)

func TestNextExhaustRequestAdvancesTopologyVersion(t *testing.T) {
	processID := wirebson.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	request := wire.MustOpMsg(
		"hello", int32(1),
		"topologyVersion", wirebson.MustDocument("processId", processID, "counter", int64(4)),
		"maxAwaitTimeMS", int64(10_000),
		"$db", "admin",
	)
	request.Flags = wire.OpMsgFlags(wire.OpMsgExhaustAllowed)
	response := wire.MustOpMsg(
		"topologyVersion", wirebson.MustDocument("processId", processID, "counter", int64(5)),
		"ok", float64(1),
	)
	response.Flags = wire.OpMsgFlags(wire.OpMsgMoreToCome)

	nextBody, err := nextExhaustRequest(request, response)
	if err != nil {
		t.Fatal(err)
	}
	next := nextBody.(*wire.OpMsg)
	if next.Flags != request.Flags {
		t.Fatalf("next flags = %s, want %s", next.Flags, request.Flags)
	}
	raw, err := next.RawDocument()
	if err != nil {
		t.Fatal(err)
	}
	document, err := raw.Decode()
	if err != nil {
		t.Fatal(err)
	}
	topologyRaw := document.Get("topologyVersion").(wirebson.RawDocument)
	topology, err := topologyRaw.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if counter := topology.Get("counter"); counter != int64(5) {
		t.Fatalf("next topology counter = %v, want 5", counter)
	}
}
