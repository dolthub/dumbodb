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
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/replication/membership"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

const logicalTimeKeyRefreshInterval = time.Minute

type logicalTimeSigningKey struct {
	id        int64
	key       []byte
	expiresAt types.Timestamp
}

func (h *Handler) withLogicalTime(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	doc, err := opMsgDocument(msg)
	if err != nil {
		return nil, err
	}
	timestamp := h.nextLogicalTime()
	doc.Set("operationTime", timestamp)
	clusterTime := must.NotFail(types.NewDocument("clusterTime", timestamp))
	if !h.internalMemberAuthenticated(ctx) {
		signature := types.Binary{Subtype: types.BinaryGeneric, B: make([]byte, sha1.Size)}
		keyID := int64(0)
		if h.externalClientNeedsSignedLogicalTime(ctx) {
			key, found, keyErr := h.currentLogicalTimeSigningKey(ctx, timestamp)
			if keyErr != nil {
				return nil, keyErr
			}
			if found {
				signature.B = signLogicalTime(timestamp, key.key)
				keyID = key.id
			}
		}
		clusterTime.Set("signature", must.NotFail(types.NewDocument("hash", signature, "keyId", keyID)))
	}
	doc.Set("$clusterTime", clusterTime)
	response, err := documentOpMsg(doc)
	if err != nil {
		return nil, err
	}
	response.Flags = msg.Flags
	return response, nil
}

func (h *Handler) externalClientNeedsSignedLogicalTime(ctx context.Context) bool {
	info := conninfo.GetIfPresent(ctx)
	return info != nil && info.Authenticated() && h.MembershipCredentials != nil && h.b != nil
}

func (h *Handler) currentLogicalTimeSigningKey(
	ctx context.Context,
	logicalTime types.Timestamp,
) (logicalTimeSigningKey, bool, error) {
	h.logicalTimeKeyMu.Lock()
	defer h.logicalTimeKeyMu.Unlock()

	now := time.Now()
	if uint64(h.logicalTimeKey.expiresAt) >= uint64(logicalTime) && now.Before(h.logicalTimeKeyRefreshAfter) {
		return h.logicalTimeKey, true, nil
	}
	key, found, err := h.loadLogicalTimeSigningKey(ctx, logicalTime)
	if err != nil {
		if uint64(h.logicalTimeKey.expiresAt) >= uint64(logicalTime) {
			return h.logicalTimeKey, true, nil
		}
		return logicalTimeSigningKey{}, false, err
	}
	if !found {
		h.logicalTimeKey = logicalTimeSigningKey{}
		h.logicalTimeKeyRefreshAfter = now.Add(time.Second)
		return logicalTimeSigningKey{}, false, nil
	}
	h.logicalTimeKey = key
	h.logicalTimeKeyRefreshAfter = now.Add(logicalTimeKeyRefreshInterval)
	return key, true, nil
}

func (h *Handler) loadLogicalTimeSigningKey(
	ctx context.Context,
	logicalTime types.Timestamp,
) (logicalTimeSigningKey, bool, error) {
	database, err := h.b.Database("admin")
	if err != nil {
		return logicalTimeSigningKey{}, false, err
	}
	collection, err := database.Collection("system.keys")
	if err != nil {
		return logicalTimeSigningKey{}, false, err
	}
	result, err := collection.Query(ctx, &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("purpose", "HMAC")),
	})
	if err != nil {
		return logicalTimeSigningKey{}, false, err
	}
	defer result.Iter.Close()

	var selected logicalTimeSigningKey
	for {
		_, document, nextErr := result.Iter.Next()
		if errors.Is(nextErr, iterator.ErrIteratorDone) {
			return selected, len(selected.key) != 0, nil
		}
		if nextErr != nil {
			return logicalTimeSigningKey{}, false, nextErr
		}
		key, matches, parseErr := parseLogicalTimeSigningKey(document)
		if parseErr != nil {
			return logicalTimeSigningKey{}, false, parseErr
		}
		if !matches || uint64(key.expiresAt) < uint64(logicalTime) {
			continue
		}
		if uint64(key.expiresAt) > uint64(selected.expiresAt) ||
			key.expiresAt == selected.expiresAt && key.id > selected.id {
			selected = key
		}
	}
}

func parseLogicalTimeSigningKey(document *types.Document) (logicalTimeSigningKey, bool, error) {
	purpose, err := document.Get("purpose")
	if err != nil || purpose != "HMAC" {
		return logicalTimeSigningKey{}, false, nil
	}
	idValue, err := document.Get("_id")
	id, ok := idValue.(int64)
	if err != nil || !ok {
		return logicalTimeSigningKey{}, false, fmt.Errorf("admin.system.keys _id has type %T, want int64", idValue)
	}
	keyValue, err := document.Get("key")
	key, ok := keyValue.(types.Binary)
	if err != nil || !ok || key.Subtype != types.BinaryGeneric || len(key.B) != sha1.Size {
		return logicalTimeSigningKey{}, false, fmt.Errorf("admin.system.keys key is not a %d-byte generic binary value", sha1.Size)
	}
	expiresValue, err := document.Get("expiresAt")
	expiresAt, ok := expiresValue.(types.Timestamp)
	if err != nil || !ok {
		return logicalTimeSigningKey{}, false, fmt.Errorf("admin.system.keys expiresAt has type %T, want timestamp", expiresValue)
	}
	return logicalTimeSigningKey{id: id, key: append([]byte(nil), key.B...), expiresAt: expiresAt}, true, nil
}

func signLogicalTime(logicalTime types.Timestamp, key []byte) []byte {
	var message [8]byte
	binary.LittleEndian.PutUint64(message[:], uint64(logicalTime)|0xffff)
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(message[:])
	return mac.Sum(nil)
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
	if !clusterTime.Has("signature") && h.membershipAuthenticationEnabled() && !h.internalMemberAuthenticated(ctx) {
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

func (h *Handler) membershipAuthenticationEnabled() bool {
	return h.NewOpts != nil && h.MembershipCredentials != nil
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
