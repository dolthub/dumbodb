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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestLogCommand(t *testing.T) {
	msg := must.NotFail(wire.NewOpMsg(wirebson.MustDocument("find", "coll", "$db", "db")))
	h := &Handler{NewOpts: &NewOpts{SlowOpThreshold: 100 * time.Millisecond}}

	for name, tc := range map[string]struct {
		level    slog.Level
		duration time.Duration
		err      error
		want     string
	}{
		"fast at info":         {slog.LevelInfo, time.Millisecond, nil, ""},
		"fast error at info":   {slog.LevelInfo, time.Millisecond, errors.New("boom"), ""},
		"fast at debug":        {slog.LevelDebug, time.Millisecond, nil, "level=DEBUG msg=command "},
		"fast error at debug":  {slog.LevelDebug, time.Millisecond, errors.New("boom"), `level=DEBUG msg="command error"`},
		"slow at info":         {slog.LevelInfo, 150 * time.Millisecond, nil, `level=INFO msg="Slow query"`},
		"slow error at info":   {slog.LevelInfo, 150 * time.Millisecond, errors.New("boom"), `level=INFO msg="Slow query"`},
		"threshold not slow":   {slog.LevelInfo, 100 * time.Millisecond, nil, ""},
		"slow at warn level":   {slog.LevelWarn, 150 * time.Millisecond, nil, ""},
		"namespace and fields": {slog.LevelDebug, time.Millisecond, nil, "cmd=find db=db ns=db.coll duration_ms=1"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level}))
			h.logCommand(conninfo.Ctx(context.Background(), conninfo.New()), l, msg, tc.duration, tc.err)
			if tc.want == "" {
				require.Empty(t, buf.String())
				return
			}
			require.Contains(t, buf.String(), tc.want)
		})
	}
}
