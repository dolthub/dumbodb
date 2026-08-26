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

import "testing"

func TestParseRemoteURL(t *testing.T) {
	// Pin the dolt home so scheme-less expansion always uses the default
	// DoltHub host regardless of the host machine's dolt config.
	t.Setenv("DOLT_ROOT_PATH", t.TempDir())

	const doltHub = "https://doltremoteapi.dolthub.com/"

	cases := []struct {
		name       string
		in         string
		wantErr    bool
		wantScheme string
		wantRaw    string // if set, the expanded/normalized Raw must match
		supported  bool
	}{
		{name: "file absolute", in: "file:///srv/backups/mydb", wantScheme: "file", supported: true},
		{name: "file uppercase scheme", in: "FILE:///srv/x", wantScheme: "file", supported: true},
		{name: "mem test-only", in: "mem://unit-test", wantScheme: "mem", supported: true},
		{name: "https gRPC supported", in: "https://dolthub.com/org/repo", wantScheme: "https", supported: true},
		{name: "http gRPC supported", in: "http://example.com/x", wantScheme: "http", supported: true},
		{name: "s3 generic object store", in: "s3://bucket/path/to/db", wantScheme: "s3", supported: true},
		{name: "s3 with routing query preserved", in: "s3://bucket/db?endpoint=http://localhost:9000&path-style=true", wantScheme: "s3", wantRaw: "s3://bucket/db?endpoint=http://localhost:9000&path-style=true", supported: true},
		{name: "localbs test blobstore", in: "localbs:///srv/bs/db", wantScheme: "localbs", supported: true},
		{name: "aws known but unsupported", in: "aws://table/bucket/db", wantScheme: "aws"},
		{name: "gs known but unsupported", in: "gs://bucket/db", wantScheme: "gs"},
		{name: "ssh known but unsupported", in: "ssh://host/path", wantScheme: "ssh"},

		// Scheme-less shorthand, matching the dolt CLI.
		{name: "dolthub org/repo shorthand", in: "macneale/dumbodb-01", wantScheme: "https", wantRaw: doltHub + "macneale/dumbodb-01", supported: true},
		{name: "bare word is a dolthub path", in: "origin", wantScheme: "https", wantRaw: doltHub + "origin", supported: true},
		{name: "scheme-less host keeps host", in: "example.com/o/r", wantScheme: "https", wantRaw: "https://example.com/o/r", supported: true},
		{name: "scheme-less host with port", in: "localhost:50051/o/r", wantScheme: "https", wantRaw: "https://localhost:50051/o/r", supported: true},

		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   ", wantErr: true},
		{name: "unknown scheme ftp", in: "ftp://host/x", wantErr: true},
		{name: "unparseable missing scheme", in: "://x", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRemoteURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseRemoteURL(%q) = %+v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRemoteURL(%q) unexpected error: %v", tc.in, err)
			}
			if got.Scheme != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", got.Scheme, tc.wantScheme)
			}
			if tc.wantRaw != "" && got.Raw != tc.wantRaw {
				t.Errorf("Raw = %q, want %q", got.Raw, tc.wantRaw)
			}
			if got.supported() != tc.supported {
				t.Errorf("supported() = %v, want %v", got.supported(), tc.supported)
			}
		})
	}
}
