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
