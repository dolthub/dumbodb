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

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// server is a dumbodb subprocess the soak owns end to end.
type server struct {
	cmd      *exec.Cmd
	addr     string
	pid      int
	dataDir  string
	keepData bool
	logPath  string

	waited   chan struct{} // closed once the process has been reaped
	state    *os.ProcessState
	stopping atomic.Bool // set before we send our own SIGTERM
}

// reap waits on the process exactly once and records its exit state.
// state is written before waited is closed, so any reader that first
// observes waited sees a consistent state.
func (s *server) reap() {
	_ = s.cmd.Wait()
	s.state = s.cmd.ProcessState
	close(s.waited)
}

// died returns a channel closed when the process exits, for whatever
// reason (crash or a stop we initiated).
func (s *server) died() <-chan struct{} { return s.waited }

func (s *server) hasExited() bool {
	select {
	case <-s.waited:
		return true
	default:
		return false
	}
}

// crashed reports whether the process exited on its own rather than in
// response to a stop we initiated.
func (s *server) crashed() bool { return s.hasExited() && !s.stopping.Load() }

// exitStatus describes how the process exited. A signal (especially
// SIGKILL) points at the OOM killer; a plain non-zero exit code with a
// panic in the log points at a Go panic.
func (s *server) exitStatus() string {
	if !s.hasExited() {
		return "still running"
	}
	if s.state == nil {
		return "exited (status unavailable)"
	}
	if ws, ok := s.state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		sig := ws.Signal()
		hint := ""
		if sig == syscall.SIGKILL {
			hint = " (likely OOM killer)"
		}
		return fmt.Sprintf("killed by signal %d/%s%s", int(sig), sig, hint)
	}
	return fmt.Sprintf("exit code %d", s.state.ExitCode())
}

// logTail returns the last n lines of the server log, for folding into
// the soak's own durable stdout when the server dies.
func (s *server) logTail(n int) []string {
	f, err := os.Open(s.logPath)
	if err != nil {
		return []string{fmt.Sprintf("(log unavailable: %v)", err)}
	}
	defer f.Close()
	ring := make([]string, 0, n)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, scanner.Text())
	}
	return ring
}

// startServer starts dumbodb on the given port with a fresh data
// dir, waits for the port to accept TCP connections, and returns the
// running server handle. dataDir is created if empty; binaryPath
// defaults to "dumbodb" on $PATH if empty.
func startServer(binaryPath, extraArgs, dataDir string, port int, keepData bool, readyTimeout time.Duration) (*server, error) {
	if binaryPath == "" {
		binaryPath = "dumbodb"
	}
	resolved, err := exec.LookPath(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("dumbodb binary %q: %w", binaryPath, err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	if dataDir == "" {
		dataDir, err = os.MkdirTemp("", "dumbodb-soak-*")
		if err != nil {
			return nil, fmt.Errorf("mkdir tempdir: %w", err)
		}
	}

	logPath := dataDir + ".log"
	logFile, err := os.Create(logPath)
	if err != nil {
		if !keepData {
			os.RemoveAll(dataDir)
		}
		return nil, fmt.Errorf("create log file: %w", err)
	}

	args := []string{"-addr", addr, "-data-dir", dataDir}
	if extraArgs != "" {
		args = append(args, strings.Fields(extraArgs)...)
	}
	cmd := exec.Command(resolved, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// New process group so a Ctrl-C on the soak doesn't propagate
	// before we have a chance to drain dumbodb cleanly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		if !keepData {
			os.RemoveAll(dataDir)
		}
		return nil, fmt.Errorf("start dumbodb: %w", err)
	}

	s := &server{
		cmd:      cmd,
		addr:     "mongodb://" + addr,
		pid:      cmd.Process.Pid,
		dataDir:  dataDir,
		keepData: keepData,
		logPath:  logPath,
		waited:   make(chan struct{}),
	}
	go s.reap()

	if err := waitForListen(s, addr, readyTimeout); err != nil {
		s.stop(5 * time.Second)
		return nil, err
	}
	return s, nil
}

// waitForListen polls TCP-connect against addr until it succeeds or
// the timeout elapses, also detecting an early process exit so we
// don't poll forever on a dumbodb that crashed at startup.
func waitForListen(s *server, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if s.hasExited() {
			return fmt.Errorf("dumbodb exited during startup (%s); see log %s", s.exitStatus(), s.logPath)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dumbodb did not start listening on %s within %s", addr, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// stop sends SIGTERM, waits up to graceTimeout for clean exit, then
// SIGKILLs the process group. Deletes the data dir unless keepData
// was set at startup.
func (s *server) stop(graceTimeout time.Duration) {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	if !s.hasExited() {
		s.stopping.Store(true)
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-s.waited:
		case <-time.After(graceTimeout):
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			<-s.waited
		}
	}

	if !s.keepData {
		os.RemoveAll(s.dataDir)
		// Preserve the log when the server died on its own so the
		// crash output survives for post-mortem analysis.
		if !s.crashed() {
			os.Remove(s.logPath)
		}
	}
}

// waitOnContext arranges for stop to fire either on ctx cancellation
// or on the returned func, whichever happens first. Safe to call the
// returned func more than once.
func (s *server) waitOnContext(ctx context.Context, graceTimeout time.Duration) func() {
	var once sync.Once
	doStop := func() {
		once.Do(func() { s.stop(graceTimeout) })
	}
	go func() {
		<-ctx.Done()
		doStop()
	}()
	return doStop
}
