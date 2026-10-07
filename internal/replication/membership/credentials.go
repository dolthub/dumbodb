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
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/xdg-go/scram"
	"gopkg.in/yaml.v3"
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
	stored  map[string][]scram.StoredCredentials
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
	var document yaml.Node
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return nil, fmt.Errorf("key file %q is not valid YAML: %w", path, err)
	}
	var secrets []string
	if len(document.Content) != 1 {
		return nil, fmt.Errorf("key file %q contains no keys", path)
	}
	if err := appendKeyFileSecrets(document.Content[0], &secrets); err != nil {
		return nil, fmt.Errorf("key file %q: %w", path, err)
	}
	return NewKeys(secrets)
}

func New(secret string) (*Credentials, error) {
	return NewKeys([]string{secret})
}

func NewKeys(secrets []string) (*Credentials, error) {
	if len(secrets) == 0 || len(secrets) > 2 {
		return nil, fmt.Errorf("key file must contain one or two keys")
	}
	normalizedSecrets := make([]string, len(secrets))
	for index, secret := range secrets {
		normalizedSecrets[index] = stripWhitespace(secret)
		if err := validateSecret(normalizedSecrets[index]); err != nil {
			return nil, err
		}
	}
	clients := make(map[string]*scram.Client, 2)
	stored := make(map[string][]scram.StoredCredentials, 2)
	for _, mechanism := range []string{Mechanism, MechanismSHA1} {
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
		for index, secret := range normalizedSecrets {
			client, err := newClient(mechanism, secret)
			if err != nil {
				return nil, err
			}
			if index == 0 {
				clients[mechanism] = client
			}
			verifier, err := client.GetStoredCredentialsWithError(scram.KeyFactors{Salt: string(salt), Iters: iterations})
			if err != nil {
				return nil, fmt.Errorf("creating internal SCRAM verifier: %w", err)
			}
			stored[mechanism] = append(stored[mechanism], verifier)
		}
	}
	return &Credentials{clients: clients, stored: stored}, nil
}

func validateSecret(secret string) error {
	if len(secret) < minimumKeyLength || len(secret) > maximumKeyLength {
		return fmt.Errorf("key file content must be between %d and %d characters after removing whitespace", minimumKeyLength, maximumKeyLength)
	}
	for _, value := range secret {
		if !isBase64Character(value) {
			return fmt.Errorf("key file content contains a character outside the base64 character set")
		}
	}
	return nil
}

func newClient(mechanism, secret string) (*scram.Client, error) {
	var generator scram.HashGeneratorFcn
	password := secret
	if mechanism == MechanismSHA1 {
		generator = scram.SHA1
		digest := md5.Sum([]byte(Username + ":mongo:" + secret))
		password = hex.EncodeToString(digest[:])
	} else {
		generator = scram.SHA256
	}
	client, err := generator.NewClient(Username, password, "")
	if err != nil {
		return nil, fmt.Errorf("creating internal SCRAM credentials: %w", err)
	}
	return client, nil
}

func (c *Credentials) NewConversation() *scram.ClientConversation {
	return c.clients[Mechanism].NewConversation()
}

func (c *Credentials) StoredCredentials(mechanism string) (scram.StoredCredentials, bool) {
	stored := c.stored[mechanism]
	if len(stored) == 0 {
		return scram.StoredCredentials{}, false
	}
	return stored[0], true
}

func stripWhitespace(value string) string {
	result := make([]byte, 0, len(value))
	for index := range len(value) {
		character := value[index]
		if character != ' ' && (character < '\t' || character > '\r') {
			result = append(result, character)
		}
	}
	return string(result)
}

func appendKeyFileSecrets(node *yaml.Node, secrets *[]string) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*secrets = append(*secrets, node.Value)
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := appendKeyFileSecrets(child, secrets); err != nil {
				return err
			}
		}
	default:
		return errors.New("must contain only scalar keys or sequences of scalar keys")
	}
	return nil
}

type ServerConversation struct {
	conversations []*scram.ServerConversation
	response      string
	valid         bool
}

func (c *Credentials) NewServerConversation(mechanism string) (*ServerConversation, error) {
	credentials := c.stored[mechanism]
	if len(credentials) == 0 {
		return nil, fmt.Errorf("unsupported internal SCRAM mechanism %q", mechanism)
	}
	nonceBytes := make([]byte, 24)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, fmt.Errorf("creating internal SCRAM nonce: %w", err)
	}
	nonce := base64.StdEncoding.EncodeToString(nonceBytes)
	var generator scram.HashGeneratorFcn
	if mechanism == MechanismSHA1 {
		generator = scram.SHA1
	} else {
		generator = scram.SHA256
	}
	conversation := &ServerConversation{}
	for _, credential := range credentials {
		credential := credential
		server, err := generator.NewServer(func(string) (scram.StoredCredentials, error) {
			return credential, nil
		})
		if err != nil {
			return nil, err
		}
		server.WithNonceGenerator(func() string { return nonce })
		conversation.conversations = append(conversation.conversations, server.NewConversation())
	}
	return conversation, nil
}

func (c *ServerConversation) Step(challenge string) (string, error) {
	var firstError error
	for _, conversation := range c.conversations {
		response, err := conversation.Step(challenge)
		if err == nil {
			if c.response == "" {
				c.response = response
			}
			if conversation.Valid() {
				c.valid = true
				c.response = response
			}
		} else if firstError == nil {
			firstError = err
		}
	}
	if c.response == "" || c.Done() && !c.valid {
		return "", firstError
	}
	response := c.response
	c.response = ""
	return response, nil
}

func (c *ServerConversation) Done() bool {
	return len(c.conversations) != 0 && c.conversations[0].Done()
}

func (c *ServerConversation) Valid() bool { return c.valid }

func (c *ServerConversation) Username() string {
	if len(c.conversations) == 0 {
		return ""
	}
	return c.conversations[0].Username()
}

func isBase64Character(value rune) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '+' || value == '/' || value == '='
}
