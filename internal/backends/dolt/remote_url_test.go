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
	cases := []struct {
		name       string
		in         string
		wantErr    bool
		wantScheme string
		supported  bool
	}{
		{name: "file absolute", in: "file:///srv/backups/mydb", wantScheme: "file", supported: true},
		{name: "file uppercase scheme", in: "FILE:///srv/x", wantScheme: "file", supported: true},
		{name: "mem test-only", in: "mem://unit-test", wantScheme: "mem", supported: true},
		{name: "https gRPC supported", in: "https://dolthub.com/org/repo", wantScheme: "https", supported: true},
		{name: "http gRPC supported", in: "http://example.com/x", wantScheme: "http", supported: true},
		{name: "aws known but unsupported", in: "aws://table/bucket/db", wantScheme: "aws"},
		{name: "gs known but unsupported", in: "gs://bucket/db", wantScheme: "gs"},
		{name: "ssh known but unsupported", in: "ssh://host/path", wantScheme: "ssh"},

		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   ", wantErr: true},
		{name: "no scheme relative path", in: "srv/backups/mydb", wantErr: true},
		{name: "bare word", in: "origin", wantErr: true},
		{name: "unknown scheme s3", in: "s3://bucket/db", wantErr: true},
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
			if got.supported() != tc.supported {
				t.Errorf("supported() = %v, want %v", got.supported(), tc.supported)
			}
		})
	}
}
