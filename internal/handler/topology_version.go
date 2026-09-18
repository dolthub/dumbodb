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
	"math"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type topologyVersion struct {
	processID types.ObjectID
	counter   int64
}

func (h *Handler) BumpTopologyVersion() {
	h.topologyMu.Lock()
	defer h.topologyMu.Unlock()
	h.topologyCounter++
	close(h.topologyChanged)
	h.topologyChanged = make(chan struct{})
}

func (h *Handler) topologyVersionDocument() *types.Document {
	h.topologyMu.Lock()
	defer h.topologyMu.Unlock()
	return topologyVersionDocument(topologyVersion{processID: h.processID, counter: h.topologyCounter})
}

func topologyVersionDocument(version topologyVersion) *types.Document {
	return must.NotFail(types.NewDocument(
		"processId", version.processID,
		"counter", version.counter,
	))
}

func (h *Handler) awaitTopologyVersion(ctx context.Context, request *types.Document) error {
	topologyValue, _ := request.Get("topologyVersion")
	maxAwaitValue, _ := request.Get("maxAwaitTimeMS")
	hasTopologyVersion := topologyValue != nil
	hasMaxAwaitTime := maxAwaitValue != nil
	if !hasTopologyVersion && !hasMaxAwaitTime {
		return nil
	}
	if !hasTopologyVersion || !hasMaxAwaitTime {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrorCode(31368),
			"topologyVersion and maxAwaitTimeMS must be specified together",
		)
	}
	requested, err := parseTopologyVersion(topologyValue)
	if err != nil {
		return err
	}
	maxAwaitTime, err := parseMaxAwaitTime(maxAwaitValue)
	if err != nil {
		return err
	}

	h.topologyMu.Lock()
	current := topologyVersion{processID: h.processID, counter: h.topologyCounter}
	changed := h.topologyChanged
	h.topologyMu.Unlock()
	if requested.processID != current.processID {
		return nil
	}
	if requested.counter > current.counter {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrorCode(31382),
			fmt.Sprintf("Received a topology version with counter: %d which is greater than the server topology version counter: %d", requested.counter, current.counter),
		)
	}
	if requested.counter < current.counter {
		return nil
	}

	timer := time.NewTimer(maxAwaitTime)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-timer.C:
		return nil
	}
}

func validateExhaustHello(message *wire.OpMsg, request *types.Document) error {
	if !message.Flags.FlagSet(wire.OpMsgExhaustAllowed) {
		return nil
	}
	topologyValue, _ := request.Get("topologyVersion")
	maxAwaitValue, _ := request.Get("maxAwaitTimeMS")
	hasTopologyVersion := topologyValue != nil
	hasMaxAwaitTime := maxAwaitValue != nil
	if hasTopologyVersion && hasMaxAwaitTime {
		return nil
	}
	return handlererrors.NewCommandErrorMsg(
		handlererrors.ErrorCode(51756),
		"An isMaster or hello request with exhaust must specify topologyVersion and maxAwaitTimeMS",
	)
}

func parseTopologyVersion(value any) (topologyVersion, error) {
	document, ok := value.(*types.Document)
	if !ok {
		return topologyVersion{}, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "topologyVersion must be an object")
	}
	processValue, _ := document.Get("processId")
	processID, ok := processValue.(types.ObjectID)
	if !ok {
		return topologyVersion{}, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "topologyVersion.processId must be an ObjectId")
	}
	counterValue, _ := document.Get("counter")
	counter, ok := counterValue.(int64)
	if !ok {
		return topologyVersion{}, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "topologyVersion.counter must be a 64-bit integer")
	}
	if counter < 0 {
		return topologyVersion{}, handlererrors.NewCommandErrorMsg(handlererrors.ErrorCode(31372), "topologyVersion must have a non-negative counter")
	}
	return topologyVersion{processID: processID, counter: counter}, nil
}

func parseMaxAwaitTime(value any) (time.Duration, error) {
	var milliseconds int64
	switch value := value.(type) {
	case int32:
		milliseconds = int64(value)
	case int64:
		milliseconds = value
	default:
		return 0, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "maxAwaitTimeMS must be an integer")
	}
	if milliseconds < 0 {
		return 0, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "maxAwaitTimeMS must be non-negative")
	}
	if milliseconds > math.MaxInt64/int64(time.Millisecond) {
		return 0, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "maxAwaitTimeMS is too large")
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}
