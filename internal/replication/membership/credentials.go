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
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"unicode"

	"github.com/xdg-go/scram"
)

const (
	Database      = "local"
	Username      = "__system"
	Mechanism     = "SCRAM-SHA-256"
	MechanismSHA1 = "SCRAM-SHA-1"

	minimumKeyLength = 6
	maximumKeyLength = 1024
	sha256Iterations = 15000
	sha1Iterations   = 10000
)

type Credentials struct {
	clients map[string]*scram.Client
	stored  map[string]scram.StoredCredentials
}

func LoadKeyFile(path string) (*Credentials, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("key file %q is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("key file %q must not have group or world permissions", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	return New(stripWhitespace(string(contents)))
}

func New(secret string) (*Credentials, error) {
	if len(secret) < minimumKeyLength || len(secret) > maximumKeyLength {
		return nil, fmt.Errorf("key file content must be between %d and %d characters after removing whitespace", minimumKeyLength, maximumKeyLength)
	}
	for _, value := range secret {
		if !isBase64Character(value) {
			return nil, fmt.Errorf("key file content contains a character outside the base64 character set")
		}
	}
	sha256Client, err := scram.SHA256.NewClient(Username, secret, "")
	if err != nil {
		return nil, fmt.Errorf("creating internal SCRAM credentials: %w", err)
	}
	digest := md5.Sum([]byte(Username + ":mongo:" + secret))
	sha1Client, err := scram.SHA1.NewClient(Username, hex.EncodeToString(digest[:]), "")
	if err != nil {
		return nil, fmt.Errorf("creating internal SCRAM credentials: %w", err)
	}
	clients := map[string]*scram.Client{
		Mechanism:     sha256Client,
		MechanismSHA1: sha1Client,
	}
	stored := make(map[string]scram.StoredCredentials, len(clients))
	for mechanism, client := range clients {
		iterations := sha256Iterations
		saltLength := 28
		if mechanism == MechanismSHA1 {
			iterations = sha1Iterations
			saltLength = 16
		}
		salt := make([]byte, saltLength)
		if _, err := rand.Read(salt); err != nil {
			return nil, fmt.Errorf("creating internal SCRAM salt: %w", err)
		}
		verifier, err := client.GetStoredCredentialsWithError(scram.KeyFactors{Salt: string(salt), Iters: iterations})
		if err != nil {
			return nil, fmt.Errorf("creating internal SCRAM verifier: %w", err)
		}
		stored[mechanism] = verifier
	}
	return &Credentials{clients: clients, stored: stored}, nil
}

func (c *Credentials) NewConversation() *scram.ClientConversation {
	return c.clients[Mechanism].NewConversation()
}

func (c *Credentials) StoredCredentials(mechanism string) (scram.StoredCredentials, bool) {
	stored, ok := c.stored[mechanism]
	return stored, ok
}

func stripWhitespace(value string) string {
	result := make([]rune, 0, len(value))
	for _, character := range value {
		if !unicode.IsSpace(character) {
			result = append(result, character)
		}
	}
	return string(result)
}

func isBase64Character(value rune) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '+' || value == '/' || value == '='
}
