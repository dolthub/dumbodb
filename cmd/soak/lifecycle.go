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
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	}

	if err := waitForListen(addr, readyTimeout, cmd); err != nil {
		s.stop(5 * time.Second)
		return nil, err
	}
	return s, nil
}

// waitForListen polls TCP-connect against addr until it succeeds or
// the timeout elapses, also detecting an early process exit so we
// don't poll forever on a dumbodb that crashed at startup.
func waitForListen(addr string, timeout time.Duration, cmd *exec.Cmd) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return fmt.Errorf("dumbodb exited during startup (exit code %d); see log", cmd.ProcessState.ExitCode())
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
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)

	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(graceTimeout):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}

	if !s.keepData {
		os.RemoveAll(s.dataDir)
		os.Remove(s.logPath)
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
