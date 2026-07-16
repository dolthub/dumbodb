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

package metrics

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	eventsapi "github.com/dolthub/eventsapi_schema/dolt/services/eventsapi/v1alpha1"
)

// captureServer is an in-process ClientEventsService that records every request.
type captureServer struct {
	eventsapi.UnimplementedClientEventsServiceServer

	mu   sync.Mutex
	reqs []*eventsapi.LogEventsRequest
	got  chan struct{}
}

func (s *captureServer) LogEvents(_ context.Context, req *eventsapi.LogEventsRequest) (*eventsapi.LogEventsResponse, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()

	select {
	case s.got <- struct{}{}:
	default:
	}

	return &eventsapi.LogEventsResponse{}, nil
}

func (s *captureServer) requests() []*eventsapi.LogEventsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*eventsapi.LogEventsRequest(nil), s.reqs...)
}

func testReporter(t *testing.T) (*captureServer, *reporter) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	capture := &captureServer{got: make(chan struct{}, 16)}
	eventsapi.RegisterClientEventsServiceServer(srv, capture)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	})

	r := &reporter{
		client:    eventsapi.NewClientEventsServiceClient(conn),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		version:   "test-version",
		machineID: "test-machine-id",
	}

	return capture, r
}

func TestSendTagsEventAsDumboDB(t *testing.T) {
	capture, r := testReporter(t)

	r.send(context.Background(), eventsapi.ClientEventType_SQL_SERVER)

	reqs := capture.requests()
	require.Len(t, reqs, 1)

	req := reqs[0]
	require.Equal(t, eventsapi.AppID_APP_DUMBODB, req.App)
	require.Equal(t, "test-version", req.Version)
	require.Equal(t, "test-machine-id", req.MachineId)
	require.Equal(t, platform(), req.Platform)
	require.Len(t, req.Events, 1)
	require.Equal(t, eventsapi.ClientEventType_SQL_SERVER, req.Events[0].Type)
	require.NotEmpty(t, req.Events[0].Id)
}

func TestRunEmitsBootThenHeartbeats(t *testing.T) {
	capture, r := testReporter(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.run(ctx, 10*time.Millisecond)
		close(done)
	}()

	waitForRequests(t, capture, 3, 2*time.Second)
	cancel()
	<-done

	reqs := capture.requests()
	require.GreaterOrEqual(t, len(reqs), 3)
	require.Equal(t, eventsapi.ClientEventType_SQL_SERVER, reqs[0].Events[0].Type, "first event must be the boot event")
	for _, req := range reqs[1:] {
		require.Equal(t, eventsapi.ClientEventType_SQL_SERVER_HEARTBEAT, req.Events[0].Type, "events after boot must be heartbeats")
	}
}

func TestRunReporterDisabledIsNoop(t *testing.T) {
	// Must return promptly without panicking and without dialing.
	RunReporter(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "v", false)
}

func TestAnonymousMachineIDStable(t *testing.T) {
	require.NotEmpty(t, anonymousMachineID())
	require.Equal(t, anonymousMachineID(), anonymousMachineID(), "machine id must be stable across calls")
}

func waitForRequests(t *testing.T, s *captureServer, n int, timeout time.Duration) {
	t.Helper()

	deadline := time.After(timeout)
	for len(s.requests()) < n {
		select {
		case <-s.got:
		case <-deadline:
			t.Fatalf("timed out waiting for %d requests, got %d", n, len(s.requests()))
		}
	}
}
