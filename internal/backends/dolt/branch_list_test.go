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
	"context"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
)

func TestDumboDBBranchListAdmin(t *testing.T) {
	backend := newTestBackend(t)
	database, err := backend.Database("admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateCollection(context.Background(), &backends.CreateCollectionParams{Name: "events"}); err != nil {
		t.Fatal(err)
	}
	result, err := backend.DumboDBBranch(context.Background(), &backends.BranchParams{
		DBName: "admin", Action: "list",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Branches) != 1 || result.Branches[0].Name != "main" {
		t.Fatalf("admin branches = %+v, want main", result.Branches)
	}
}

func TestDumboDBBranchConfigurationOnAdmin(t *testing.T) {
	ctx := context.Background()
	backend := newTestBackend(t)
	database, err := backend.Database("admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "events"}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.DumboDBRemote(ctx, &backends.RemoteParams{
		DBName: "admin", Action: "add", Name: "origin", URL: "file://" + t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	pullRemote := "origin"
	pullBranch := "main"
	if _, err := backend.DumboDBBranch(ctx, &backends.BranchParams{
		DBName: "admin", Action: "add", From: "main", Name: "feature",
		ConfigUpdate: &backends.BranchConfigUpdate{PullRemote: &pullRemote, PullBranch: &pullBranch},
	}); err != nil {
		t.Fatal(err)
	}
	pushRemote := "origin"
	pushBranch := "feature"
	if _, err := backend.DumboDBBranch(ctx, &backends.BranchParams{
		DBName: "admin", Action: "update", Name: "feature",
		ConfigUpdate: &backends.BranchConfigUpdate{PushRemote: &pushRemote, PushBranch: &pushBranch},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.DumboDBBranch(ctx, &backends.BranchParams{
		DBName: "admin", Action: "remove", From: "main", Name: "feature", Force: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := backend.readBranchConfig(ctx, "admin", "feature"); err != nil || ok {
		t.Fatalf("deleted admin branch config remains: ok=%v, err=%v", ok, err)
	}
	rebase := "true"
	if _, err := backend.DumboDBBranch(ctx, &backends.BranchParams{
		DBName: "admin", Action: "add", From: "main", Name: "invalid",
		ConfigUpdate: &backends.BranchConfigUpdate{PullRebase: &rebase},
	}); err == nil {
		t.Fatal("admin branch with invalid configuration succeeded")
	}
	branches, err := backend.DumboDBBranch(ctx, &backends.BranchParams{DBName: "admin", Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range branches.Branches {
		if branch.Name == "invalid" {
			t.Fatal("failed configured branch creation left the branch behind")
		}
	}
}
