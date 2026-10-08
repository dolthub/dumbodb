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

package clientconn

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
	"github.com/stretchr/testify/require"
)

func TestRedactedBodyHidesSecrets(t *testing.T) {
	nonce := []byte("n,,n=u,r=SecretNonceValue")
	for _, c := range []struct {
		msg    *wire.OpMsg
		secret []string
		keep   string
	}{
		{wire.MustOpMsg("createUser", "u", "pwd", "s3cret-pw", "roles", wirebson.MakeArray(0), "$db", "admin"), []string{"s3cret-pw"}, "createUser"},
		{wire.MustOpMsg("updateUser", "u", "pwd", "an0ther-pw", "$db", "admin"), []string{"an0ther-pw"}, "updateUser"},
		{wire.MustOpMsg("saslStart", int32(1), "mechanism", "SCRAM-SHA-256", "payload", wirebson.Binary{B: nonce}, "$db", "admin"),
			[]string{"SecretNonceValue", base64.StdEncoding.EncodeToString(nonce)}, "saslStart"},
		{wire.MustOpMsg("dumboRemote", int32(1), "action", "add", "url", "https://someone:t0ken@example.com/r", "$db", "x"), []string{"t0ken"}, "example.com"},
		{wire.MustOpMsg("users", wirebson.MustArray(wirebson.MustDocument("user", "u", "credentials",
			wirebson.MustDocument("SCRAM-SHA-256", wirebson.MustDocument("storedKey", "StoredKeyValue")))), "ok", float64(1)),
			[]string{"StoredKeyValue"}, "users"},
	} {
		out := redactedBody(c.msg)
		for _, s := range c.secret {
			require.NotContains(t, out, s)
		}
		require.Contains(t, out, c.keep)
	}
}

// The debug dump runs before the command's own nesting check, so it must not
// recurse without bound either.
func TestRedactedBodyHandlesDeepNesting(t *testing.T) {
	const depth = 2_000_000
	const levelOverhead = 4 + 3 + 1
	raw := make([]byte, 0, depth*levelOverhead+5)
	for level := range depth - 1 {
		raw = binary.LittleEndian.AppendUint32(raw, uint32((depth-level-1)*levelOverhead+5))
		raw = append(raw, 0x03, 'a', 0)
	}
	raw = append(raw, 5, 0, 0, 0, 0)
	for range depth - 1 {
		raw = append(raw, 0)
	}
	msg, err := wire.NewOpMsg(wirebson.RawDocument(raw))
	require.NoError(t, err)

	done := make(chan string, 1)
	go func() { done <- redactedBody(msg) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rendering a deeply nested body did not finish")
	}
}
