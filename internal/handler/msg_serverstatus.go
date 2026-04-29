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
	"os"
	"path/filepath"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/version"
	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgServerStatus implements `serverStatus` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgServerStatus(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	exec, err := os.Executable()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	uptime := time.Since(h.StateProvider.Get().Start)

	metricsDoc := types.MakeDocument(0)

	stats, err := h.b.Status(connCtx, new(backends.StatusParams))
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	res := must.NotFail(types.NewDocument(
		"host", host,
		"version", version.Get().MongoDBVersion,
		"process", filepath.Base(exec),
		"pid", int64(os.Getpid()),
		"uptime", uptime.Seconds(),
		"uptimeMillis", uptime.Milliseconds(),
		"uptimeEstimate", int64(uptime.Seconds()),
		"localTime", time.Now(),
		"metrics", must.NotFail(types.NewDocument(
			"commands", metricsDoc,
		)),
		"catalogStats", must.NotFail(types.NewDocument(
			"collections", int32(stats.CountCollections),
			"capped", stats.CountCappedCollections,
			"clustered", int32(0),
			"timeseries", int32(0),
			"views", int32(0),
			"internalCollections", int32(0),
			"internalViews", int32(0),
		)),
		"ok", float64(1),
	))

	return documentOpMsg(
		res,
	)
}
