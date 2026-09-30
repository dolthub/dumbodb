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

	"github.com/FerretDB/wire"
)

func (h *Handler) postProcessResponse(ctx context.Context, request, response *wire.OpMsg) *wire.OpMsg {
	current := h.applyResponsePostProcessor(ctx, "write concern", response, func() (*wire.OpMsg, error) {
		return withWriteConcernResult(request, response)
	})
	current = h.applyResponsePostProcessor(ctx, "logical time", current, func() (*wire.OpMsg, error) {
		return h.withLogicalTime(current)
	})
	return current
}

func (h *Handler) applyResponsePostProcessor(
	ctx context.Context,
	stage string,
	response *wire.OpMsg,
	process func() (*wire.OpMsg, error),
) *wire.OpMsg {
	processed, err := process()
	if err != nil {
		h.logPostProcessError(ctx, stage, err)
		return response
	}
	return processed
}

func (h *Handler) logPostProcessError(ctx context.Context, stage string, err error) {
	if h.L != nil {
		h.L.WarnContext(ctx, "response post-processing failed", "stage", stage, "error", err)
	}
}
