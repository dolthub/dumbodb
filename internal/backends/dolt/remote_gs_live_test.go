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

// TestDumboDBPushFetch_GSLive pushes to and fetches from a Google Cloud Storage
// bucket over the gs:// scheme. Gated on DUMBO_GS_TEST_URL so it skips wherever
// no bucket or credentials are configured; credentials come from the standard
// GCS client chain (GOOGLE_APPLICATION_CREDENTIALS, gcloud ADC, metadata) or, for
// an emulator, STORAGE_EMULATOR_HOST.
//
// DUMBO_GS_TEST_URL is a full gs:// URL naming an existing bucket and a path,
// e.g. gs://my-bucket/dumbo-test. A per-run branch keeps concurrent runs and
// re-runs independent.
func TestDumboDBPushFetch_GSLive(t *testing.T) {
	base := os.Getenv("DUMBO_GS_TEST_URL")
	if base == "" {
		t.Skip("DUMBO_GS_TEST_URL not set; skipping live gs integration test")
	}
	if !strings.HasPrefix(base, "gs://") {
		t.Fatalf("DUMBO_GS_TEST_URL must be a gs:// URL, got %q", base)
	}

	ctx := context.Background()
	branch := fmt.Sprintf("dumbo-gs-%d", time.Now().UnixNano())

	src := newTestBackend(t)
	insertDoc(t, src, "srcdb", "col", mustDoc(t, "_id", int64(1), "v", int64(11)))
	want := commitDB(t, src, "srcdb", "seed on main")
	if _, err := src.DumboDBBranch(ctx, &backends.BranchParams{DBName: "srcdb", From: "main", Name: branch}); err != nil {
		t.Fatalf("create branch %s: %v", branch, err)
	}

	if _, err := src.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "srcdb", Action: "add", Name: "origin", URL: base}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	pres, err := src.DumboDBPush(ctx, &backends.PushParams{DBName: "srcdb", Remote: "origin", Branch: branch, BranchExplicit: true})
	if err != nil {
		t.Fatalf("push to gs %s: %v", base, err)
	}
	if pres.Commit != want {
		t.Errorf("pushed commit = %s, want %s", pres.Commit, want)
	}
	t.Logf("pushed %s @ %s to %s", branch, want, base)

	dst := newTestBackend(t)
	insertDoc(t, dst, "dstdb", "col", mustDoc(t, "_id", int64(99)))
	commitDB(t, dst, "dstdb", "seed")
	if _, err := dst.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "dstdb", Action: "add", Name: "origin", URL: base}); err != nil {
		t.Fatalf("add remote (dst): %v", err)
	}
	fres, err := dst.DumboDBFetch(ctx, &backends.FetchParams{DBName: "dstdb", Remote: "origin"})
	if err != nil {
		t.Fatalf("fetch from gs: %v", err)
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
