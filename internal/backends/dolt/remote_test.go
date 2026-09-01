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

func TestDumboDBRemote(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	rp := func(action, name, url string) *backends.RemoteParams {
		return &backends.RemoteParams{DBName: "mydb", Action: action, Name: name, URL: url}
	}

	// add
	res, err := b.DumboDBRemote(ctx, rp("add", "origin", "file:///srv/backups/mydb"))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(res.Remotes) != 1 || res.Remotes[0].Name != "origin" || res.Remotes[0].URL != "file:///srv/backups/mydb" {
		t.Fatalf("add result = %+v", res)
	}

	// list returns the added remote
	res, err = b.DumboDBRemote(ctx, rp("list", "", ""))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Remotes) != 1 || res.Remotes[0].Name != "origin" {
		t.Fatalf("list = %+v", res)
	}

	// db scoping: another database sees nothing
	res, err = b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "otherdb", Action: "list"})
	if err != nil {
		t.Fatalf("otherdb list: %v", err)
	}
	if len(res.Remotes) != 0 {
		t.Errorf("otherdb list = %+v, want empty", res.Remotes)
	}

	// duplicate add rejected
	if _, err = b.DumboDBRemote(ctx, rp("add", "origin", "file:///other")); err == nil {
		t.Error("duplicate add: want error, got nil")
	}

	// invalid url rejected (unparseable; a bare word would be valid DoltHub
	// shorthand, so use something url.Parse actually rejects)
	if _, err = b.DumboDBRemote(ctx, rp("add", "bad", "://x")); err == nil {
		t.Error("invalid url: want error, got nil")
	}

	// invalid name rejected
	if _, err = b.DumboDBRemote(ctx, rp("add", "a@b", "file:///y")); err == nil {
		t.Error("name with '@': want error, got nil")
	}
	if _, err = b.DumboDBRemote(ctx, rp("add", "a b", "file:///y")); err == nil {
		t.Error("name with whitespace: want error, got nil")
	}

	// remove
	if _, err = b.DumboDBRemote(ctx, rp("remove", "origin", "")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	res, err = b.DumboDBRemote(ctx, rp("list", "", ""))
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if len(res.Remotes) != 0 {
		t.Errorf("list after remove = %+v, want empty", res.Remotes)
	}

	// remove nonexistent rejected
	if _, err = b.DumboDBRemote(ctx, rp("remove", "ghost", "")); err == nil {
		t.Error("remove nonexistent: want error, got nil")
	}

	// unknown action rejected
	if _, err = b.DumboDBRemote(ctx, rp("frobnicate", "", "")); err == nil {
		t.Error("unknown action: want error, got nil")
	}
}
