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
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) withLogicalTime(msg *wire.OpMsg) (*wire.OpMsg, error) {
	doc, err := opMsgDocument(msg)
	if err != nil {
		return nil, err
	}
	timestamp := h.nextLogicalTime()
	doc.Set("operationTime", timestamp)
	doc.Set("$clusterTime", must.NotFail(types.NewDocument(
		"clusterTime", timestamp,
		"signature", must.NotFail(types.NewDocument(
			"hash", types.Binary{Subtype: types.BinaryGeneric, B: make([]byte, 20)},
			"keyId", int64(0),
		)),
	)))
	response, err := documentOpMsg(doc)
	if err != nil {
		return nil, err
	}
	response.Flags = msg.Flags
	return response, nil
}

func (h *Handler) nextLogicalTime() types.Timestamp {
	candidate := uint64(types.NewTimestamp(time.Now().UTC(), 1))
	for {
		previous := h.logicalTime.Load()
		if candidate <= previous {
			candidate = previous + 1
		}
		if h.logicalTime.CompareAndSwap(previous, candidate) {
			return types.Timestamp(candidate)
		}
	}
}
