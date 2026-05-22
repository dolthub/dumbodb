// Copyright 2021 FerretDB Inc.
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

// Package registry provides a registry of handlers.
package registry

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/dolthub/dumbodb/internal/handler"
	"github.com/dolthub/dumbodb/internal/util/password"
	"github.com/dolthub/dumbodb/internal/util/state"
)

type newHandlerFunc func(opts *NewHandlerOpts) (*handler.Handler, CloseBackendFunc, error)

type CloseBackendFunc func()

// registry maps handler names to constructors.
//
// Map values must be added through the `init()` functions in separate files
// so that we can control which handlers will be included in the build with build tags.
var registry = map[string]newHandlerFunc{}

// NewHandlerOpts represents configuration for constructing handlers.
type NewHandlerOpts struct {
	// for all backends
	Logger        *slog.Logger
	StateProvider *state.Provider
	TCPHost       string
	ReplSetName   string
	SetupDatabase string
	SetupUsername string
	SetupPassword password.Password
	SetupTimeout  time.Duration

	// DoltDataDir is the directory where dolt backend stores its data.
	// Used only by the "dolt" handler.
	DoltDataDir string

	// AutoCommit, when true, automatically creates a Dolt commit after every
	// document write (insert/update/delete). Useful for legacy applications that
	// want detailed write-level history without explicit doltCommit calls.
	// Used only by the "dolt" handler.
	AutoCommit bool

	// SessionIsolation, when true, runs DumboDB in version-control-native
	// isolation: writes auto-fork into a per-connection working-set overlay,
	// startTransaction is rejected with code 263, and doltCommit merges the
	// overlay back to the branch HEAD with three-way conflict detection.
	SessionIsolation bool

	// SessionTimeout overrides the default lsid-keyed session idle
	// timeout. Zero leaves the dolt backend's default in place (30m).
	SessionTimeout time.Duration

	// SessionSweepPeriod overrides the default registry sweep cadence.
	// Zero leaves the dolt backend's default in place (1m).
	SessionSweepPeriod time.Duration

	TestOpts

	_ struct{} // prevent unkeyed literals
}

// TestOpts represents experimental configuration options.
type TestOpts struct {
	DisablePushdown         bool
	EnableNestedPushdown    bool
	CappedCleanupInterval   time.Duration
	CappedCleanupPercentage uint8
	EnableNewAuth           bool
	BatchSize               int
	MaxBsonObjectSizeBytes  int
	_                       struct{} // prevent unkeyed literals
}

// NewHandler constructs a new handler.
//
// The caller is responsible to call CloseBackendFunc when the handler is no longer needed.
func NewHandler(name string, opts *NewHandlerOpts) (*handler.Handler, CloseBackendFunc, error) {
	if opts == nil {
		return nil, nil, fmt.Errorf("opts is nil")
	}

	newHandler := registry[name]
	if newHandler == nil {
		return nil, nil, fmt.Errorf("unknown handler %q", name)
	}

	return newHandler(opts)
}

