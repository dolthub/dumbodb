// Copyright 2024 Dolt Inc.
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
	"github.com/dolthub/dongo/internal/backends/stub"
	"github.com/dolthub/dongo/internal/handler"
)

func init() {
	registry["stub"] = newStubHandler
}

func newStubHandler(opts *NewHandlerOpts) (*handler.Handler, CloseBackendFunc, error) {
	b, err := stub.NewBackend(opts.Logger)
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
		ConnMetrics:   opts.ConnMetrics,
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
		return nil, nil, err
	}

	return h, b.Close, nil
}
