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
	"fmt"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/replication/membership"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) withLogicalTime(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	doc, err := opMsgDocument(msg)
	if err != nil {
		return nil, err
	}
	timestamp := h.nextLogicalTime()
	doc.Set("operationTime", timestamp)
	clusterTime := must.NotFail(types.NewDocument("clusterTime", timestamp))
	if !h.internalMemberAuthenticated(ctx) {
		clusterTime.Set("signature", must.NotFail(types.NewDocument(
			"hash", types.Binary{Subtype: types.BinaryGeneric, B: make([]byte, 20)},
			"keyId", int64(0),
		)))
	}
	doc.Set("$clusterTime", clusterTime)
	response, err := documentOpMsg(doc)
	if err != nil {
		return nil, err
	}
	response.Flags = msg.Flags
	return response, nil
}

func (h *Handler) internalMemberAuthenticated(ctx context.Context) bool {
	info := conninfo.GetIfPresent(ctx)
	if info == nil || !info.Authenticated() {
		return false
	}
	username, _, _, database := info.Auth()
	return username == membership.Username && database == membership.Database
}

func (h *Handler) observeLogicalTime(ctx context.Context, msg *wire.OpMsg) error {
	document, err := opMsgDocument(msg)
	if err != nil {
		return err
	}
	value, err := document.Get("$clusterTime")
	if err != nil {
		return nil
	}
	clusterTime, ok := value.(*types.Document)
	if !ok {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "$clusterTime must be an object")
	}
	if !clusterTime.Has("signature") && !h.internalMemberAuthenticated(ctx) {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrUnauthorized, "unsigned $clusterTime requires internal membership authentication")
	}
	timestamp, err := clusterTime.Get("clusterTime")
	if err != nil {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "$clusterTime.clusterTime is required")
	}
	logicalTime, ok := timestamp.(types.Timestamp)
	if !ok {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "$clusterTime.clusterTime must be a timestamp")
	}
	seconds := int64(uint64(logicalTime) >> 32)
	if seconds > time.Now().Add(365*24*time.Hour).Unix() {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, fmt.Sprintf("$clusterTime is too far in the future: %d", seconds))
	}
	for {
		previous := h.logicalTime.Load()
		if uint64(logicalTime) <= previous || h.logicalTime.CompareAndSwap(previous, uint64(logicalTime)) {
			return nil
		}
	}
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
