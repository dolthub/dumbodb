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

package handler

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dolthub/dumbodb/internal/backends/dolt"
)

func handlerForTest(t *testing.T) *Handler {
	t.Helper()
	be, err := dolt.NewBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	if err != nil {
		t.Fatalf("dolt.NewBackend: %v", err)
	}
	t.Cleanup(be.Close)
	h := &Handler{NewOpts: &NewOpts{Backend: be, L: slog.New(slog.NewTextHandler(io.Discard, nil))}, b: be}
	h.initCommands()
	return h
}

func TestCommands_DurableFlag(t *testing.T) {
	h := handlerForTest(t)
	cmds := h.Commands()

	durableNames := []string{"doltCommit", "dumboCommit", "commitTransaction", "doltGC", "dumboGC"}
	for _, n := range durableNames {
		cmd, ok := cmds[n]
		assert.True(t, ok, "command %q must be registered", n)
		if ok {
			assert.True(t, cmd.Durable, "command %q must have Durable=true", n)
		}
	}

	for name, cmd := range cmds {
		if contains(durableNames, name) {
			continue
		}
		assert.False(t, cmd.Durable, "command %q must not have Durable=true", name)
	}
}

func TestCommands_BlockedInTxnFlag(t *testing.T) {
	h := handlerForTest(t)
	cmds := h.Commands()

	blockedNames := []string{"drop", "dropDatabase", "createIndexes", "renameCollection", "collMod"}
	for _, n := range blockedNames {
		cmd, ok := cmds[n]
		assert.True(t, ok, "command %q must be registered", n)
		if ok {
			assert.True(t, cmd.BlockedInTxn, "command %q must have BlockedInTxn=true", n)
		}
	}

	for name, cmd := range cmds {
		if contains(blockedNames, name) {
			continue
		}
		assert.False(t, cmd.BlockedInTxn, "command %q must not have BlockedInTxn=true", name)
	}
}

// Aliases must share the same *Command instance so adding a flag in one
// place automatically applies to every name the command answers to.
func TestCommands_AliasesShareInstance(t *testing.T) {
	h := handlerForTest(t)
	cmds := h.Commands()

	aliasGroups := [][]string{
		{"buildInfo", "buildinfo"},
		{"dbStats", "dbstats"},
		{"findAndModify", "findandmodify"},
		{"isMaster", "ismaster"},
		{"doltCommit", "dumboCommit"},
		{"doltBranch", "dumboBranch"},
		{"doltBranchStatus", "dumboBranchStatus"},
		{"doltCherryPick", "dumboCherryPick"},
		{"doltConflicts", "dumboConflicts"},
		{"doltDiff", "dumboDiff"},
		{"doltLog", "dumboLog"},
		{"doltMerge", "dumboMerge"},
		{"doltRebase", "dumboRebase"},
		{"doltReset", "dumboReset"},
		{"doltResolveConflict", "dumboResolveConflict"},
		{"doltRevert", "dumboRevert"},
		{"doltStatus", "dumboStatus"},
		{"doltTag", "dumboTag"},
		{"doltGC", "dumboGC"},
	}

	for _, group := range aliasGroups {
		first, ok := cmds[group[0]]
		assert.True(t, ok, "command %q must be registered", group[0])
		for _, other := range group[1:] {
			cmd, ok := cmds[other]
			assert.True(t, ok, "command %q must be registered", other)
			assert.Same(t, first, cmd, "%q and %q must share the same *Command", group[0], other)
		}
	}
}

// Durable and BlockedInTxn are independent properties and no current
// command sets both.
func TestCommands_NoCommandHasBothFlags(t *testing.T) {
	h := handlerForTest(t)
	for name, cmd := range h.Commands() {
		assert.False(t, cmd.Durable && cmd.BlockedInTxn,
			"command %q has both Durable and BlockedInTxn", name)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
