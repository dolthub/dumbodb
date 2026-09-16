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

package topology

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/replication/control"
)

const (
	defaultPositionReportInterval = 2 * time.Second
	defaultPositionReportTimeout  = 15 * time.Second
)

// ProgressReporter sends durable replication positions to the current sync source.
type ProgressReporter struct {
	manager   *Manager
	logger    *slog.Logger
	interval  time.Duration
	timeout   time.Duration
	newClient func(string) heartbeatClient

	mu           sync.Mutex
	requested    uint64
	acknowledged uint64
	wake         chan struct{}
}

func NewProgressReporter(manager *Manager, logger *slog.Logger) *ProgressReporter {
	if logger == nil {
		logger = slog.Default()
	}
	reporter := &ProgressReporter{
		manager:  manager,
		logger:   logger,
		interval: defaultPositionReportInterval,
		timeout:  defaultPositionReportTimeout,
		newClient: func(host string) heartbeatClient {
			return newConnectorClient(host, manager.Snapshot().MemberHost)
		},
		wake: make(chan struct{}, 1),
	}
	manager.SetProgressListener(reporter.Notify)
	return reporter
}

// Notify requests a report after a durable checkpoint changes.
func (r *ProgressReporter) Notify() {
	r.mu.Lock()
	r.requested++
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *ProgressReporter) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	var client heartbeatClient
	var source string
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
		for {
			state := r.manager.Snapshot()
			if state.SyncSource == "" || state.Configuration == nil {
				break
			}
			if source != state.SyncSource {
				if client != nil {
					_ = client.Close()
				}
				source = state.SyncSource
				client = r.newClient(source)
			}
			generation := r.requestedGeneration()
			reportContext, cancel := context.WithTimeout(ctx, r.timeout)
			err := sendProgressReport(reportContext, client, state)
			cancel()
			if err != nil {
				r.logger.Warn("replication position report failed", "source", source, "err", err)
				_ = client.Close()
				client = nil
				source = ""
				break
			}
			if !r.acknowledge(generation) {
				continue
			}
			break
		}
	}
}

func (r *ProgressReporter) requestedGeneration() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requested
}

func (r *ProgressReporter) acknowledge(generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation > r.acknowledged {
		r.acknowledged = generation
	}
	return r.requested == r.acknowledged
}

func sendProgressReport(ctx context.Context, client heartbeatClient, state Snapshot) error {
	response, err := client.Request(ctx, progressReportRequest(state))
	if err != nil {
		return err
	}
	document, err := response.RawDocument()
	if err != nil {
		return fmt.Errorf("reading replSetUpdatePosition response: %w", err)
	}
	decoded, err := document.Decode()
	if err != nil {
		return fmt.Errorf("decoding replSetUpdatePosition response: %w", err)
	}
	if ok, _ := decoded.Get("ok").(float64); ok != 1 {
		return fmt.Errorf("replSetUpdatePosition failed: %v", decoded.Get("errmsg"))
	}
	return nil
}

func progressReportRequest(state Snapshot) *wire.OpMsg {
	positions := wirebson.MakeArray(1 + len(state.Members))
	_ = positions.Add(progressPosition(state.MemberID, state.Configuration.Version, state.Checkpoint.Written, state.Checkpoint.Applied, state.Checkpoint.Durable))
	memberIDs := make([]int, 0, len(state.Members))
	for memberID, member := range state.Members {
		if member.Healthy {
			memberIDs = append(memberIDs, memberID)
		}
	}
	slices.Sort(memberIDs)
	for _, memberID := range memberIDs {
		member := state.Members[memberID]
		_ = positions.Add(progressPosition(memberID, state.Configuration.Version, member.Written, member.Applied, member.Durable))
	}
	return wire.MustOpMsg(
		"replSetUpdatePosition", int32(1),
		"optimes", positions,
		"maxTimeMSOpOnly", int64(defaultPositionReportTimeout/time.Millisecond),
		"$replData", int32(1),
		"$db", "admin",
	)
}

func progressPosition(memberID int, configurationVersion int64, written, applied, durable control.OpTime) *wirebson.Document {
	position := wirebson.MakeDocument(8)
	_ = position.Add("writtenOpTime", progressOpTime(written))
	_ = position.Add("writtenWallTime", progressWallTime(written))
	_ = position.Add("appliedOpTime", progressOpTime(applied))
	_ = position.Add("appliedWallTime", progressWallTime(applied))
	_ = position.Add("durableOpTime", progressOpTime(durable))
	_ = position.Add("durableWallTime", progressWallTime(durable))
	_ = position.Add("memberId", int32(memberID))
	_ = position.Add("cfgver", configurationVersion)
	return position
}

func progressOpTime(opTime control.OpTime) *wirebson.Document {
	document := wirebson.MakeDocument(2)
	_ = document.Add("ts", wirebson.Timestamp(uint64(opTime.Seconds)<<32|uint64(opTime.Increment)))
	_ = document.Add("t", opTime.Term)
	return document
}

func progressWallTime(opTime control.OpTime) time.Time {
	return time.Unix(int64(opTime.Seconds), 0).UTC()
}
