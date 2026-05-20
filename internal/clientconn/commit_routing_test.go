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

package clientconn

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// .6.4.10 acceptance: the wire-dispatch helper recognises the
// durable-commit commands so they are routed through Shadow.Commit
// (writeMu fence) rather than Shadow.Use (lock-free hot path).

func TestCommandIsDurableCommit(t *testing.T) {
	durable := []string{"doltCommit", "commitTransaction"}
	for _, c := range durable {
		assert.True(t, commandIsDurableCommit(c), "%q must route through Shadow.Commit", c)
	}

	notDurable := []string{
		"find", "insert", "update", "delete",
		"hello", "ping", "isMaster", "buildInfo",
		"abortTransaction", // releases the txn; not a durable boundary
		"endSessions",      // advisory; not durable
		"doltBranch",       // metadata mutation; current design does not need the fence
	}
	for _, c := range notDurable {
		assert.False(t, commandIsDurableCommit(c), "%q must NOT route through Shadow.Commit", c)
	}
}
