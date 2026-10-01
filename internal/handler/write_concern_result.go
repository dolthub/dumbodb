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
	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func withWriteConcernResult(request, response *wire.OpMsg) (*wire.OpMsg, error) {
	requestDoc, err := opMsgDocument(request)
	if err != nil {
		return nil, err
	}
	writeConcern, _ := requestDoc.Get("writeConcern")
	decision := common.DecideWriteConcern(writeConcern)
	if !decision.Unsatisfiable && !decision.UnknownTag {
		return response, nil
	}

	responseDoc, err := opMsgDocument(response)
	if err != nil {
		return nil, err
	}
	code := int32(100)
	codeName := "UnsatisfiableWriteConcern"
	message := "Not enough data-bearing nodes"
	if decision.UnknownTag {
		code = 79
		codeName = "UnknownReplWriteConcern"
		message = "No write concern mode named '" + writeConcernTag(writeConcern) + "' found in replica set configuration"
	}
	responseDoc.Set("writeConcernError", must.NotFail(types.NewDocument(
		"code", code,
		"codeName", codeName,
		"errmsg", message,
	)))
	result, err := documentOpMsg(responseDoc)
	if err != nil {
		return nil, err
	}
	result.Flags = response.Flags
	return result, nil
}

func writeConcernTag(value any) string {
	doc, ok := value.(*types.Document)
	if !ok {
		return ""
	}
	tag, _ := doc.Get("w")
	name, _ := tag.(string)
	return name
}
