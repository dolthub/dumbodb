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
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/FerretDB/wire"
)

func TestApplyResponsePostProcessorPreservesSuccessfulResponseOnError(t *testing.T) {
	h := &Handler{NewOpts: &NewOpts{L: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	response := &wire.OpMsg{}

	got := h.applyResponsePostProcessor(context.Background(), "forced failure", response, func() (*wire.OpMsg, error) {
		return nil, errors.New("forced opMsgDocument failure")
	})
	if got != response {
		t.Fatal("post-processing replaced the successful response")
	}
}
