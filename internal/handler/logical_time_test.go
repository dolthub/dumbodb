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
	"testing"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestWithLogicalTime(t *testing.T) {
	h := &Handler{}
	response := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1)))))

	first, err := h.withLogicalTime(response)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.withLogicalTime(response)
	if err != nil {
		t.Fatal(err)
	}

	firstTime := responseLogicalTime(t, first)
	secondTime := responseLogicalTime(t, second)
	if secondTime <= firstTime {
		t.Fatalf("logical time did not advance: first=%v second=%v", firstTime, secondTime)
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
