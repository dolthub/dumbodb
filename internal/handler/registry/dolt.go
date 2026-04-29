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

package registry

import (
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/handler"
)

func init() {
	registry["dolt"] = newDoltHandler
}

func newDoltHandler(opts *NewHandlerOpts) (*handler.Handler, CloseBackendFunc, error) {
	dataDir := opts.DoltDataDir
	if dataDir == "" {
		dataDir = "data"
	}

	b, err := dolt.NewBackend(dataDir, opts.Logger, opts.AutoCommit)
	if err != nil {
		return nil, nil, err
	}

	h, err := handler.New(&handler.NewOpts{
		Backend:     b,
		TCPHost:     opts.TCPHost,
		ReplSetName: opts.ReplSetName,

		SetupDatabase: opts.SetupDatabase,
		SetupUsername: opts.SetupUsername,
		SetupPassword: opts.SetupPassword,
		SetupTimeout:  opts.SetupTimeout,

		L:             opts.Logger,
		StateProvider: opts.StateProvider,

		DisablePushdown:         opts.TestOpts.DisablePushdown,
		EnableNestedPushdown:    opts.TestOpts.EnableNestedPushdown,
		CappedCleanupInterval:   opts.TestOpts.CappedCleanupInterval,
		CappedCleanupPercentage: opts.TestOpts.CappedCleanupPercentage,
		EnableNewAuth:           opts.TestOpts.EnableNewAuth,
		BatchSize:               opts.TestOpts.BatchSize,
		MaxBsonObjectSizeBytes:  opts.TestOpts.MaxBsonObjectSizeBytes,
	})
	if err != nil {
		b.Close()
		return nil, nil, err
	}

	return h, b.Close, nil
}
