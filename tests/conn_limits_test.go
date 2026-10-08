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

package tests

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// selfSignedCertKeyFile writes a self-signed certificate and key for
// 127.0.0.1 as one PEM file and returns its path.
func selfSignedCertKeyFile(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	contents := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...)
	path := filepath.Join(t.TempDir(), "server.pem")
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}

func helloOnRawConn(t *testing.T, addr string) (net.Conn, error) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	w := &wireConn{c: c}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = w.run(bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}})
	_ = c.SetDeadline(time.Time{})
	return c, err
}

// Before the fix the server accepted any number of connections, each with
// its own goroutine, so a client could exhaust file descriptors and memory.
// Like mongod's --maxConns, connections beyond the limit are closed.
func TestMaxConnsRefusesExtraConnections(t *testing.T) {
	env := startDumboDB(t, "--maxConns", "2")
	addr := fmt.Sprintf("127.0.0.1:%d", env.Port)

	var open []net.Conn
	refused := 0
	for range 5 {
		c, err := helloOnRawConn(t, addr)
		if err != nil {
			refused++
			continue
		}
		open = append(open, c)
	}
	require.LessOrEqual(t, len(open), 2, "at most --maxConns connections are served")
	require.Positive(t, refused)

	for _, c := range open {
		_ = c.Close()
	}
	require.Eventually(t, func() bool {
		_, err := helloOnRawConn(t, addr)
		return err == nil
	}, 10*time.Second, 200*time.Millisecond, "capacity is released when connections close")
}

// Before the fix a client could open a TLS connection and never complete the
// handshake, holding the connection and its goroutine forever. (An idle
// connection that completed setup stays open, as in mongod; --maxConns
// bounds how many there can be.)
func TestSilentTLSClientIsDisconnected(t *testing.T) {
	cert := selfSignedCertKeyFile(t)
	env := startDumboDB(t, "--tlsMode", "requireTLS", "--tlsCertificateKeyFile", cert, "--tlsCAFile", cert)
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", env.Port))
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.SetReadDeadline(time.Now().Add(45*time.Second)))
	_, err = c.Read(make([]byte, 1))
	var netErr net.Error
	require.False(t, errors.As(err, &netErr) && netErr.Timeout(), "the server must close a connection that never completes the TLS handshake")
	require.ErrorIs(t, err, io.EOF)
}
