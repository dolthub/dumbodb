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

package dolt

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBackendForSweepTest(t *testing.T, period time.Duration) *Backend {
	t.Helper()
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	close(be.sweeperStop)
	<-be.sweeperDone
	be.sweeperStop = make(chan struct{})
	be.sweeperDone = make(chan struct{})
	be.sweeperPeriod = period
	go be.sessionSweepLoop()
	return be
}

func TestBackend_SessionSweep_FiresPeriodically(t *testing.T) {
	be := newBackendForSweepTest(t, 20*time.Millisecond)
	defer be.Close() //nolint:errcheck

	shadow, err := be.SessionRegistry().Connect("test-lsid")
	require.NoError(t, err)
	require.True(t, shadow.Active())
	be.SessionRegistry().End("test-lsid")

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if be.SessionRegistry().Len() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Equal(t, 0, be.SessionRegistry().Len())
	assert.False(t, shadow.Active())
}

func TestBackend_SessionSweep_StopsOnClose(t *testing.T) {
	be := newBackendForSweepTest(t, 10*time.Millisecond)

	closeStart := time.Now()
	be.Close() //nolint:errcheck
	closeDuration := time.Since(closeStart)
	assert.Less(t, closeDuration, 200*time.Millisecond)

	select {
	case <-be.sweeperDone:
	default:
		t.Fatal("sweeperDone must be closed after Backend.Close returns")
	}
}

func TestBackend_SessionSweep_EmptyRegistryNoPanic(t *testing.T) {
	be := newBackendForSweepTest(t, 5*time.Millisecond)
	defer be.Close() //nolint:errcheck

	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, 0, be.SessionRegistry().Len())
}
