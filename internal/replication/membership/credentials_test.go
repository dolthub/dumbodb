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

package membership

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadKeyFileRemovesAllWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyfile")
	if err := os.WriteFile(path, []byte("abc def\tGHI\nJKLMNOPqrstuvwx\n"), 0600); err != nil {
		t.Fatal(err)
	}
	credentials, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	conversation := credentials.NewConversation()
	first, err := conversation.Step("")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("SCRAM conversation returned an empty first message")
	}
}

func TestKeyFileValidation(t *testing.T) {
	tests := []struct {
		name   string
		secret string
	}{
		{name: "too short", secret: "abcde"},
		{name: "too long", secret: string(make([]byte, maximumKeyLength+1))},
		{name: "invalid character", secret: "abcdef!"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.secret); err == nil {
				t.Fatal("New accepted an invalid key")
			}
		})
	}
}

func TestMongoDBMechanismParameters(t *testing.T) {
	credentials, err := New("abcdefghijklmnop")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		mechanism  string
		saltLength int
		iterations int
	}{
		{mechanism: Mechanism, saltLength: 28, iterations: sha256Iterations},
		{mechanism: MechanismSHA1, saltLength: 16, iterations: sha1Iterations},
	}
	for _, test := range tests {
		stored, ok := credentials.StoredCredentials(test.mechanism)
		if !ok {
			t.Fatalf("missing %s credentials", test.mechanism)
		}
		if len(stored.Salt) != test.saltLength || stored.Iters != test.iterations {
			t.Fatalf("%s parameters = salt %d iterations %d", test.mechanism, len(stored.Salt), stored.Iters)
		}
	}
}

func TestLoadKeyFileRejectsOpenPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not use Unix file permissions")
	}
	path := filepath.Join(t.TempDir(), "keyfile")
	if err := os.WriteFile(path, []byte("abcdefghijkl"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(path); err == nil {
		t.Fatal("LoadKeyFile accepted group-readable permissions")
	}
}
