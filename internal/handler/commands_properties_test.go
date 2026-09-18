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
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/stretchr/testify/assert"

	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
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

func TestCommands_NoCustomReplicationCommands(t *testing.T) {
	h := handlerForTest(t)
	for _, name := range []string{"dumboReplicationDetach", "dumboReplicationStatus"} {
		if _, ok := h.Commands()[name]; ok {
			t.Fatalf("custom replication command %q is registered", name)
		}
	}
}

func TestCommands_ReplicationRejectsMutations(t *testing.T) {
	h := configuredReplicationHandler(t)
	h.initCommands()
	ctx := conninfo.Ctx(context.Background(), conninfo.New())
	outStage := must.NotFail(types.NewDocument("$out", "copy"))
	outPipeline := must.NotFail(types.NewArray(outStage))
	tests := []struct {
		name    string
		message *wire.OpMsg
	}{
		{name: "insert", message: wire.MustOpMsg("insert", "items", "$db", "orders")},
		{name: "update", message: wire.MustOpMsg("update", "items", "$db", "orders")},
		{name: "delete", message: wire.MustOpMsg("delete", "items", "$db", "orders")},
		{name: "findAndModify", message: wire.MustOpMsg("findAndModify", "items", "$db", "orders")},
		{name: "bulkWrite", message: wire.MustOpMsg("bulkWrite", int32(1), "$db", "admin")},
		{name: "create", message: wire.MustOpMsg("create", "items", "$db", "orders")},
		{name: "aggregate", message: commandMessage("aggregate", "items", "pipeline", outPipeline, "$db", "orders")},
		{name: "dumboBranch", message: wire.MustOpMsg("dumboBranch", int32(1), "action", "add", "branch", "other", "$db", "orders")},
		{name: "dumboCommit", message: wire.MustOpMsg("dumboCommit", int32(1), "$db", "orders")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := h.Commands()[test.name].Handler(ctx, test.message)
			commandError, ok := err.(*handlererrors.CommandError)
			if !ok {
				t.Fatalf("error = %T %v, want command error", err, err)
			}
			if commandError.Code() != handlererrors.ErrNotWritablePrimary || commandError.Err().Error() != "not primary" {
				t.Fatalf("error = %v, want NotWritablePrimary (10107): not primary", commandError)
			}
			if commandError.Code().String() != "NotWritablePrimary" {
				t.Fatalf("codeName = %q, want NotWritablePrimary", commandError.Code())
			}
		})
	}
}

func TestCommands_SecondaryMutationCommandsAreClassified(t *testing.T) {
	h := handlerForTest(t)
	mutatingCommands := []string{
		"aggregate", "bulkWrite", "convertToCapped", "create", "delete", "dropIndexes", "insert", "update",
		"findAndModify", "findandmodify", "drop", "dropDatabase", "createIndexes", "renameCollection", "collMod",
		"createUser", "dropAllUsersFromDatabase", "dropUser", "updateUser", "grantRolesToUser", "revokeRolesFromUser",
		"createRole", "updateRole", "dropRole", "dropAllRolesFromDatabase", "grantPrivilegesToRole", "revokePrivilegesFromRole",
		"grantRolesToRole", "revokeRolesFromRole", "commitTransaction",
		"doltBranch", "dumboBranch", "doltCherryPick", "dumboCherryPick", "doltMerge", "dumboMerge",
		"doltRebase", "dumboRebase", "doltReset", "dumboReset", "doltRemote", "dumboRemote", "doltPush", "dumboPush",
		"doltFetch", "dumboFetch", "doltPull", "dumboPull", "doltClone", "dumboClone",
		"doltResolveConflict", "dumboResolveConflict", "doltRevert", "dumboRevert", "doltTag", "dumboTag",
		"doltUndrop", "dumboUndrop", "doltCommit", "dumboCommit", "doltGC", "dumboGC",
	}
	for _, name := range mutatingCommands {
		command, ok := h.Commands()[name]
		if !ok || command.MutatesState == nil {
			t.Errorf("command %q has no secondary mutation classification", name)
		}
	}
}

func TestCommands_SecondaryMutationClassification(t *testing.T) {
	h := handlerForTest(t)
	emptyPipeline := must.NotFail(types.NewArray())
	outPipeline := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("$out", "copy"))))
	tests := []struct {
		name    string
		message *wire.OpMsg
		want    bool
	}{
		{name: "aggregate read", message: commandMessage("aggregate", "items", "pipeline", emptyPipeline, "$db", "orders")},
		{name: "aggregate out", message: commandMessage("aggregate", "items", "pipeline", outPipeline, "$db", "orders"), want: true},
		{name: "branch list", message: wire.MustOpMsg("dumboBranch", int32(1), "action", "list", "$db", "orders")},
		{name: "branch add", message: wire.MustOpMsg("dumboBranch", int32(1), "action", "add", "$db", "orders"), want: true},
		{name: "remote list", message: wire.MustOpMsg("dumboRemote", int32(1), "action", "list", "$db", "orders")},
		{name: "remote add", message: wire.MustOpMsg("dumboRemote", int32(1), "action", "add", "$db", "orders"), want: true},
		{name: "tag list", message: wire.MustOpMsg("dumboTag", int32(1), "$db", "orders")},
		{name: "tag add", message: wire.MustOpMsg("dumboTag", int32(1), "name", "v1", "$db", "orders"), want: true},
		{name: "undrop list", message: wire.MustOpMsg("dumboUndrop", int32(1), "$db", "admin")},
		{name: "undrop restore", message: wire.MustOpMsg("dumboUndrop", int32(1), "name", "orders", "$db", "admin"), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keys := must.NotFail(opMsgDocument(test.message)).Keys()
			command := h.Commands()[keys[0]]
			got := command.MutatesState != nil && command.MutatesState(test.message)
			if got != test.want {
				t.Fatalf("MutatesState = %v, want %v", got, test.want)
			}
		})
	}
}

func commandMessage(pairs ...any) *wire.OpMsg {
	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(pairs...))))
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
