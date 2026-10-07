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

	"github.com/xdg-go/scram"
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

func TestLoadKeyFileYAMLSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyfile")
	if err := os.WriteFile(path, []byte("- abcdefgh\n- ijklmnop\n"), 0600); err != nil {
		t.Fatal(err)
	}
	credentials, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"abcdefgh", "ijklmnop"} {
		if err := authenticate(credentials, secret); err != nil {
			t.Fatalf("key %q did not authenticate: %v", secret, err)
		}
	}
	if err := authenticateOutbound(credentials, "abcdefgh"); err != nil {
		t.Fatalf("first key was not used outbound: %v", err)
	}
	if err := authenticateOutbound(credentials, "ijklmnop"); err == nil {
		t.Fatal("second key was used outbound")
	}
}

func TestLoadKeyFileRejectsMoreThanTwoKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyfile")
	if err := os.WriteFile(path, []byte("[abcdef, ghijkl, mnopqr]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(path); err == nil {
		t.Fatal("LoadKeyFile accepted three keys")
	}
}

func authenticate(credentials *Credentials, secret string) error {
	client, err := newClient(Mechanism, secret)
	if err != nil {
		return err
	}
	server, err := credentials.NewServerConversation(Mechanism)
	if err != nil {
		return err
	}
	clientConversation := client.NewConversation()
	clientFirst, err := clientConversation.Step("")
	if err != nil {
		return err
	}
	serverFirst, err := server.Step(clientFirst)
	if err != nil {
		return err
	}
	clientFinal, err := clientConversation.Step(serverFirst)
	if err != nil {
		return err
	}
	serverFinal, err := server.Step(clientFinal)
	if err != nil {
		return err
	}
	_, err = clientConversation.Step(serverFinal)
	return err
}

func authenticateOutbound(credentials *Credentials, secret string) error {
	target, err := newClient(Mechanism, secret)
	if err != nil {
		return err
	}
	stored, err := target.GetStoredCredentialsWithError(scram.KeyFactors{Salt: "0123456789012345678901234567", Iters: sha256Iterations})
	if err != nil {
		return err
	}
	server, err := scram.SHA256.NewServer(func(string) (scram.StoredCredentials, error) { return stored, nil })
	if err != nil {
		return err
	}
	clientConversation := credentials.NewConversation()
	serverConversation := server.NewConversation()
	clientFirst, err := clientConversation.Step("")
	if err != nil {
		return err
	}
	serverFirst, err := serverConversation.Step(clientFirst)
	if err != nil {
		return err
	}
	clientFinal, err := clientConversation.Step(serverFirst)
	if err != nil {
		return err
	}
	serverFinal, err := serverConversation.Step(clientFinal)
	if err != nil {
		return err
	}
	_, err = clientConversation.Step(serverFinal)
	return err
}
