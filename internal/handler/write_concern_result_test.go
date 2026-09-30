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

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestWithWriteConcernResult(t *testing.T) {
	request := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"insert", "col",
		"writeConcern", must.NotFail(types.NewDocument("w", int32(2), "wtimeout", int32(100))),
	))))
	response := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument("n", int32(1), "ok", float64(1)))))

	got, err := withWriteConcernResult(request, response)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := opMsgDocument(got)
	if err != nil {
		t.Fatal(err)
	}
	value, err := doc.Get("writeConcernError")
	if err != nil {
		t.Fatal(err)
	}
	writeConcernError, ok := value.(*types.Document)
	if !ok {
		t.Fatalf("writeConcernError has type %T", value)
	}
	code, err := writeConcernError.Get("code")
	if err != nil {
		t.Fatal(err)
	}
	if code != int32(100) {
		t.Fatalf("code=%v", code)
	}
}
