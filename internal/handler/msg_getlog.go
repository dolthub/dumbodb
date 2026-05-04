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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/version"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/logging"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgGetLog implements `getLog` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgGetLog(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	command := document.Command()

	getLog, err := document.Get(command)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if _, ok := getLog.(types.NullType); ok {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrMissingField,
			`BSON field 'getLog.getLog' is missing but a required field`,
		)
	}

	if _, ok := getLog.(string); !ok {
		return nil, handlererrors.NewCommandError(
			handlererrors.ErrTypeMismatch,
			fmt.Errorf(
				"BSON field 'getLog.getLog' is the wrong type '%s', expected type 'string'",
				handlerparams.AliasFromType(getLog),
			),
		)
	}

	var resDoc *types.Document

	switch getLog {
	case "*":
		resDoc = must.NotFail(types.NewDocument(
			"names", must.NotFail(types.NewArray("global", "startupWarnings")),
			"ok", float64(1),
		))

	case "global":
		var res *wirebson.Array

		if res, err = logging.RecentEntries.GetArray(); err != nil {
			return nil, lazyerrors.Error(err)
		}

		resDoc = must.NotFail(types.NewDocument(
			"totalLinesWritten", int32(res.Len()),
			"log", must.NotFail(bson.ToArray(res)),
			"ok", float64(1),
		))

	case "startupWarnings":
		state := h.StateProvider.Get()

		info := version.Get()

		// it may be empty if no connection was established yet
		var b string
		if state.BackendVersion != "" {
			b, _, _ = strings.Cut(state.BackendVersion, " (")
			b = " and " + state.BackendName + " " + strings.TrimSpace(b)
		}

		startupWarnings := []string{
			fmt.Sprintf("Powered by DumboDB %s%s.", info.Commit, b),
			"Star Us! https://github.com/dolthub/dumbodb",
		}

		if h.L.Enabled(connCtx, slog.LevelDebug) {
			startupWarnings = append(startupWarnings, "Debug logging enabled. The security and performance will be affected.")
		}

		switch {
		case state.UpdateInfo != "", state.UpdateAvailable:
			msg := state.UpdateInfo
			if msg == "" {
				msg = fmt.Sprintf(
					"A new version available! The latest version: %s. The current version: %s.",
					state.LatestVersion, info.Version,
				)
			}

			startupWarnings = append(startupWarnings, msg)
		}

		var log types.Array

		for _, line := range startupWarnings {
			b, err := json.Marshal(map[string]any{
				"msg":  line,
				"tags": []string{"startupWarnings"},
				"s":    "I",
				"c":    "STORAGE",
				"id":   42000,
				"ctx":  "initandlisten",
				"t": map[string]string{
					"$date": time.Now().UTC().Format("2006-01-02T15:04:05.999Z07:00"),
				},
			})
			if err != nil {
				return nil, lazyerrors.Error(err)
			}

			log.Append(string(b))
		}
		resDoc = must.NotFail(types.NewDocument(
			"totalLinesWritten", int32(log.Len()),
			"log", &log,
			"ok", float64(1),
		))

	default:
		return nil, handlererrors.NewCommandError(
			handlererrors.ErrOperationFailed,
			fmt.Errorf("No log named '%s'", getLog),
		)
	}

	return documentOpMsg(
		resDoc,
	)
}
