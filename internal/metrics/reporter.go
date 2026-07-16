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

// Package metrics phones DumboDB usage home to the DoltHub metrics server so
// running servers can be counted. It is self-contained: its only inputs are the
// values RunReporter receives. See docs/design/metrics-phone-home.md.
package metrics

import (
	"context"
	"crypto/tls"
	"log/slog"
	"runtime"
	"time"

	"github.com/denisbrodbeck/machineid"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsapi "github.com/dolthub/eventsapi_schema/dolt/services/eventsapi/v1alpha1"
)

const (
	// defaultEndpoint is the DoltHub events API, reached over gRPC/TLS.
	defaultEndpoint = "eventsapi.dolthub.com:443"

	// machineIDApp salts the anonymous machine id so DumboDB installs occupy a
	// namespace distinct from Dolt/Doltgres, which both use "dolt".
	machineIDApp = "dumbodb"

	// heartbeatInterval is how often a running server reports that it is alive.
	heartbeatInterval = 24 * time.Hour

	// sendTimeout bounds a single phone-home so a slow network never blocks.
	sendTimeout = 1500 * time.Millisecond
)

// RunReporter emits a boot event immediately and a heartbeat every 24h until ctx
// is canceled. It is best-effort: failures are logged at debug and otherwise
// ignored, and it never panics the caller. It returns immediately if disabled.
//
// Intended to be launched as `go RunReporter(...)`.
func RunReporter(ctx context.Context, logger *slog.Logger, version string, enabled bool) {
	if !enabled {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			logger.Debug("metrics reporter panic recovered", "recover", r)
		}
	}()

	conn, err := grpc.NewClient(defaultEndpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	if err != nil {
		logger.Debug("metrics: failed to create client", "err", err)
		return
	}
	defer conn.Close()

	r := &reporter{
		client:    eventsapi.NewClientEventsServiceClient(conn),
		logger:    logger,
		version:   version,
		machineID: anonymousMachineID(),
	}

	r.run(ctx, heartbeatInterval)
}

// reporter holds the state for a single phone-home loop.
type reporter struct {
	client    eventsapi.ClientEventsServiceClient
	logger    *slog.Logger
	version   string
	machineID string
}

// run sends a boot SQL_SERVER event, then a SQL_SERVER_HEARTBEAT on every tick
// until ctx is canceled.
func (r *reporter) run(ctx context.Context, interval time.Duration) {
	r.send(ctx, eventsapi.ClientEventType_SQL_SERVER)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.send(ctx, eventsapi.ClientEventType_SQL_SERVER_HEARTBEAT)
		}
	}
}

// send emits a single event, tagged as DumboDB, with a bounded deadline.
func (r *reporter) send(ctx context.Context, eventType eventsapi.ClientEventType) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	now := timestamppb.Now()
	req := &eventsapi.LogEventsRequest{
		MachineId: r.machineID,
		Version:   r.version,
		Platform:  platform(),
		App:       eventsapi.AppID_APP_DUMBODB,
		Events: []*eventsapi.ClientEvent{
			{
				Id:        uuid.NewString(),
				StartTime: now,
				EndTime:   now,
				Type:      eventType,
			},
		},
	}

	if _, err := r.client.LogEvents(ctx, req); err != nil {
		r.logger.Debug("metrics: send failed", "type", eventType.String(), "err", err)
		return
	}

	r.logger.Debug("metrics: sent", "type", eventType.String())
}

// platform maps the build OS onto the events API Platform enum.
func platform() eventsapi.Platform {
	switch runtime.GOOS {
	case "darwin":
		return eventsapi.Platform_DARWIN
	case "linux":
		return eventsapi.Platform_LINUX
	case "windows":
		return eventsapi.Platform_WINDOWS
	default:
		return eventsapi.Platform_PLATFORM_UNSPECIFIED
	}
}

// anonymousMachineID returns a stable, one-way per-machine id keyed by
// machineIDApp. It reveals no machine GUID, hostname, or user. Falls back to
// "invalid" (the same sentinel dolt uses) if the OS id cannot be read.
func anonymousMachineID() string {
	id, err := machineid.ProtectedID(machineIDApp)
	if err != nil {
		return "invalid"
	}
	return id
}
