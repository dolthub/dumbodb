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
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
)

// TestDumboDBPushFetch_S3Live pushes to and fetches from a generic
// S3-compatible object store (AWS S3, Cloudflare R2, or a local MinIO) over the
// s3:// scheme. Gated on DUMBO_S3_TEST_URL so it skips wherever no bucket or
// credentials are configured; credentials come from the standard AWS SDK chain
// (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, shared config, IMDS, ...).
//
// DUMBO_S3_TEST_URL is a full s3:// URL naming an existing bucket and a path,
// with any routing in the query string, e.g.
//
//	s3://my-bucket/dumbo-test?endpoint=http://localhost:9000&region=us-east-1&path-style=true
//
// A per-run branch keeps concurrent runs and re-runs independent.
func TestDumboDBPushFetch_S3Live(t *testing.T) {
	base := os.Getenv("DUMBO_S3_TEST_URL")
	if base == "" {
		t.Skip("DUMBO_S3_TEST_URL not set; skipping live s3 integration test")
	}
	if !strings.HasPrefix(base, "s3://") {
		t.Fatalf("DUMBO_S3_TEST_URL must be an s3:// URL, got %q", base)
	}

	ctx := context.Background()
	branch := fmt.Sprintf("dumbo-s3-%d", time.Now().UnixNano())

	// Producer: seed on main, branch off at that commit, push the unique branch.
	src := newTestBackend(t)
	insertDoc(t, src, "srcdb", "col", mustDoc(t, "_id", int64(1), "v", int64(7)))
	want := commitDB(t, src, "srcdb", "seed on main")
	if _, err := src.DumboDBBranch(ctx, &backends.BranchParams{DBName: "srcdb", From: "main", Name: branch}); err != nil {
		t.Fatalf("create branch %s: %v", branch, err)
	}

	if _, err := src.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "srcdb", Action: "add", Name: "origin", URL: base}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	pres, err := src.DumboDBPush(ctx, &backends.PushParams{DBName: "srcdb", Remote: "origin", RefSpec: branch})
	if err != nil {
		t.Fatalf("push to s3 %s: %v", base, err)
	}
	if pres.CommitPushed != want {
		t.Errorf("pushed commit = %s, want %s", pres.CommitPushed, want)
	}
	t.Logf("pushed %s @ %s to %s", branch, want, base)

	// Consumer: fetch from the same bucket, confirm the branch came back.
	dst := newTestBackend(t)
	insertDoc(t, dst, "dstdb", "col", mustDoc(t, "_id", int64(99)))
	commitDB(t, dst, "dstdb", "seed")
	if _, err := dst.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "dstdb", Action: "add", Name: "origin", URL: base}); err != nil {
		t.Fatalf("add remote (dst): %v", err)
	}
	fres, err := dst.DumboDBFetch(ctx, &backends.FetchParams{DBName: "dstdb", Remote: "origin"})
	if err != nil {
		t.Fatalf("fetch from s3: %v", err)
	}
	var got string
	for _, br := range fres.Branches {
		if br.Branch == branch {
			got = br.Commit
		}
	}
	if got != want {
		t.Errorf("fetched branch %s = %q, want %s (branches: %+v)", branch, got, want, fres.Branches)
	}
}
