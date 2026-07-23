// Copyright 2021 FerretDB Inc.
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
	"fmt"
	"strings"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgHello implements `hello` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgHello(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	doc, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	resp, err := h.hello(connCtx, doc, h.TCPHost, h.ReplSetName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(resp)
}

// hello checks client metadata and returns hello's document fields.
// It also returns response for deprecated `isMaster` and `ismaster` commands.
func (h *Handler) hello(ctx context.Context, doc *types.Document, tcpHost, name string) (*types.Document, error) {
	if err := checkClientMetadata(ctx, doc); err != nil {
		return nil, lazyerrors.Error(err)
	}

	res := must.NotFail(types.NewDocument())

	switch doc.Command() {
	case "hello":
		res.Set("isWritablePrimary", true)
	case "isMaster", "ismaster":
		if helloOk, _ := doc.Get("helloOk"); helloOk != nil {
			res.Set("helloOk", true)
		}

		res.Set("ismaster", true)
	default:
		panic(fmt.Sprintf("unexpected command: %q", doc.Command()))
	}

	saslSupportedMechs, err := common.GetOptionalParam(doc, "saslSupportedMechs", "")
	if err != nil {
		return nil, err
	}

	var resSupportedMechs *types.Array

	if saslSupportedMechs != "" {
		db, username, ok := strings.Cut(saslSupportedMechs, ".")
		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrBadValue,
				"UserName must contain a '.' separated database.user pair",
			)
		}

		if resSupportedMechs, err = h.getUserSupportedMechs(ctx, db, username); err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

	if name != "" {
		// That does not work for TLS-only setups, IPv6 addresses, etc.
		// The proper solution is to support `replSetInitiate` command.
		if strings.HasPrefix(tcpHost, ":") {
			tcpHost = "localhost" + tcpHost
		}

		res.Set("setName", name)
		res.Set("hosts", must.NotFail(types.NewArray(tcpHost)))
	}

	res.Set("maxBsonObjectSize", int32(h.MaxBsonObjectSizeBytes))
	res.Set("maxMessageSizeBytes", int32(wire.MaxMsgLen))
	res.Set("maxWriteBatchSize", maxWriteBatchSize)
	res.Set("localTime", time.Now())
	res.Set("logicalSessionTimeoutMinutes", logicalSessionTimeoutMinutes)
	res.Set("connectionId", connectionID)
	res.Set("minWireVersion", common.MinWireVersion)
	res.Set("maxWireVersion", common.MaxWireVersion)
	res.Set("readOnly", false)
	// topologyVersion is deliberately omitted. Emitting it advertises
	// awaitable ("streaming") hello monitoring (maxWireVersion >= 9):
	// drivers then send an awaitable hello carrying maxAwaitTimeMS +
	// exhaustAllowed and expect the server to hold the request open.
	// This handler does not await, so a streaming monitor gets an
	// instant non-exhaust reply, judges the connection broken, drops it
	// every heartbeat, cycles the server to Unknown, and clears the
	// client connection pool (observed with MongoDB Compass). Without
	// topologyVersion, drivers fall back to polling monitoring, which
	// this handler serves correctly.

	if resSupportedMechs != nil && resSupportedMechs.Len() != 0 {
		res.Set("saslSupportedMechs", resSupportedMechs)
	}

	if v, _ := doc.Get("speculativeAuthenticate"); v != nil {
		authDoc, ok := v.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("speculativeAuthenticate type wrong; expected: document; got: %T", v),
			)
		}

		if specDB, dbErr := common.GetRequiredParam[string](authDoc, "db"); dbErr == nil {
			// A failed speculative attempt (unknown user, wrong mechanism) leaves
			// the field unset; the client falls back to a normal handshake.
			if specAuth, saslErr := h.saslStart(ctx, specDB, authDoc); saslErr == nil {
				res.Set("speculativeAuthenticate", specAuth)
			}
		}
	}

	res.Set("ok", float64(1))

	return res, nil
}

// getUserSupportedMechs returns supported mechanisms for the given user.
// If the user was not found, it returns nil.
func (h *Handler) getUserSupportedMechs(ctx context.Context, db, username string) (*types.Array, error) {
	adminDB, err := h.b.Database("admin")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	usersCol, err := adminDB.Collection("system.users")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	filter, err := usersInfoFilter(false, false, db, []usersInfoPair{
		{username: username, db: db},
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	qr, err := usersCol.Query(ctx, nil)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	defer qr.Iter.Close()

	for {
		_, v, err := qr.Iter.Next()

		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		matches, err := common.FilterDocument(v, filter)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if !matches {
			continue
		}

		credentialsV, _ := v.Get("credentials")
		if credentialsV == nil {
			return nil, nil
		}

		credentials := credentialsV.(*types.Document)

		supportedMechs := types.MakeArray(len(credentials.Keys()))
		for _, mechanism := range credentials.Keys() {
			supportedMechs.Append(mechanism)
		}

		return supportedMechs, nil
	}

	return nil, nil
}
