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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/youmark/pkcs8"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/dolthub/dumbodb/internal/util/tlsutil"
)

// TestPortFlag verifies that --port overrides the port in --addr.
func TestPortFlag(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dataDir := t.TempDir()
	binary := filepath.Join(repoRoot(), ".runtime", "bin", "dolt")

	// --port with no --addr exercises the port override on the default addr.
	cmd := exec.Command(binary, "--port", fmt.Sprintf("%d", port), "--data-dir", dataDir)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	client, err := mongo.Connect(options.Client().
		ApplyURI(fmt.Sprintf("mongodb://%s/", addr)).
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
	require.NoError(t, err)
	t.Cleanup(func() { client.Disconnect(context.Background()) })

	ctx := context.Background()
	var result bson.M
	require.NoError(t, client.Database("testdb").RunCommand(ctx, bson.D{
		{Key: "doltStatus", Value: int32(1)},
	}).Decode(&result))
	assert.EqualValues(t, 1, result["ok"])
}

// TestAddrFlag verifies that --addr sets the listen address.
func TestAddrFlag(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dataDir := t.TempDir()
	binary := filepath.Join(repoRoot(), ".runtime", "bin", "dolt")

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(binary, "--addr", addr, "--data-dir", dataDir)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	client, err := mongo.Connect(options.Client().
		ApplyURI(fmt.Sprintf("mongodb://%s/", addr)).
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
	require.NoError(t, err)
	t.Cleanup(func() { client.Disconnect(context.Background()) })

	ctx := context.Background()
	var result bson.M
	require.NoError(t, client.Database("testdb").RunCommand(ctx, bson.D{
		{Key: "doltStatus", Value: int32(1)},
	}).Decode(&result))
	assert.EqualValues(t, 1, result["ok"])
}

// TestAutoCommitFlag verifies that --auto-commit is accepted and causes
// writes to auto-commit. This complements TestAutoCommit in
// auto_commit_test.go by testing the flag parsing path.
func TestAutoCommitFlag(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()

	coll := env.Client.Database("flagtest").Collection("items")
	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: 1}})
	require.NoError(t, err)

	// Without explicit doltCommit, the log should have 2 entries
	// (Initialize + auto-commit from the insert).
	var raw bson.M
	require.NoError(t, env.Client.Database("flagtest").RunCommand(ctx, bson.D{
		{Key: "doltLog", Value: int32(1)},
	}).Decode(&raw))
	lr := decodeLogResult(t, raw)
	assert.GreaterOrEqual(t, len(lr.Commits), 2,
		"auto-commit must produce at least Initialize + 1 auto-insert commit")
}

// TestAutoCommitSessionIsolationMutuallyExclusive: the server refuses to start
// when both --auto-commit and --session-isolation are set.
func TestAutoCommitSessionIsolationMutuallyExclusive(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "dumbodb")
	build := exec.Command("go", "build", "-o", binary, "./cmd/dumbodb/")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building binary: %v\n%s", err, out)
	}

	cmd := exec.Command(binary, "--auto-commit", "--session-isolation", "--data-dir", t.TempDir())
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "server must exit non-zero when both flags are set")
	assert.Contains(t, string(out), "mutually exclusive",
		"error must explain the flags are mutually exclusive; got: %s", out)
}

func TestTLSFlags(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "dumbodb")
	build := exec.Command("go", "build", "-o", binary, "./cmd/dumbodb/")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building binary: %v\n%s", err, out)
	}

	certificateKeyFile, caFile, roots := writeTLSCertificates(t)
	serverTLSConfig, err := tlsutil.ServerConfig(tlsutil.ServerConfigOptions{
		CertificateFile: certificateKeyFile,
		KeyFile:         certificateKeyFile,
	})
	require.NoError(t, err)
	assert.Equal(t, tls.NoClientCert, serverTLSConfig.ClientAuth)
	mutualTLSConfig, err := tlsutil.ServerConfig(tlsutil.ServerConfigOptions{
		CertificateFile: certificateKeyFile,
		KeyFile:         certificateKeyFile,
		CAFile:          caFile,
	})
	require.NoError(t, err)
	assert.Equal(t, tls.RequireAndVerifyClientCert, mutualTLSConfig.ClientAuth)
	optionalClientCertificateConfig, err := tlsutil.ServerConfig(tlsutil.ServerConfigOptions{
		CertificateFile:                     certificateKeyFile,
		KeyFile:                             certificateKeyFile,
		CAFile:                              caFile,
		AllowConnectionsWithoutCertificates: true,
	})
	require.NoError(t, err)
	assert.Equal(t, tls.VerifyClientCertIfGiven, optionalClientCertificateConfig.ClientAuth)

	serverTLSAddr := startTLSServer(t, binary, certificateKeyFile, caFile, "requireTLS", "", true, "TLS1_2")
	client := newTLSClient(t, serverTLSAddr, roots)
	t.Cleanup(func() { client.Disconnect(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx, nil))

	plaintextClient, err := mongo.Connect(options.Client().
		ApplyURI("mongodb://" + serverTLSAddr + "/?directConnection=true").
		SetServerSelectionTimeout(time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { plaintextClient.Disconnect(context.Background()) })
	plaintextCtx, plaintextCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer plaintextCancel()
	assert.Error(t, plaintextClient.Ping(plaintextCtx, nil))

	tls12Client, err := mongo.Connect(options.Client().
		ApplyURI("mongodb://" + serverTLSAddr + "/?directConnection=true").
		SetTLSConfig(&tls.Config{
			RootCAs: roots, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
		}).
		SetServerSelectionTimeout(time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { tls12Client.Disconnect(context.Background()) })
	tls12Ctx, tls12Cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer tls12Cancel()
	assert.Error(t, tls12Client.Ping(tls12Ctx, nil))

	for _, mode := range []string{"allowTLS", "preferTLS"} {
		t.Run(mode, func(t *testing.T) {
			addr := startTLSServer(t, binary, certificateKeyFile, caFile, mode, "", true, "")
			plaintextClient, err := mongo.Connect(options.Client().
				ApplyURI("mongodb://" + addr + "/?directConnection=true").
				SetServerSelectionTimeout(time.Second))
			require.NoError(t, err)
			t.Cleanup(func() { plaintextClient.Disconnect(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			require.NoError(t, plaintextClient.Ping(ctx, nil))

			tlsClient := newTLSClient(t, addr, roots)
			t.Cleanup(func() { tlsClient.Disconnect(context.Background()) })
			tlsCtx, tlsCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer tlsCancel()
			require.NoError(t, tlsClient.Ping(tlsCtx, nil))
		})
	}

	encryptedKeyFile := encryptTLSPrivateKey(t, certificateKeyFile, "key-password")
	encryptedAddr := startTLSServer(t, binary, encryptedKeyFile, caFile, "requireTLS", "key-password", true, "")
	encryptedClient := newTLSClient(t, encryptedAddr, roots)
	t.Cleanup(func() { encryptedClient.Disconnect(context.Background()) })
	encryptedCtx, encryptedCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer encryptedCancel()
	require.NoError(t, encryptedClient.Ping(encryptedCtx, nil))
}

func startTLSServer(
	t *testing.T,
	binary string,
	certificateKeyFile string,
	caFile string,
	tlsMode string,
	keyPassword string,
	allowConnectionsWithoutCertificates bool,
	disabledProtocols string,
) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	args := []string{
		"--addr", addr,
		"--data-dir", t.TempDir(),
		"--tlsMode", tlsMode,
		"--tlsCertificateKeyFile", certificateKeyFile,
		"--tlsCAFile", caFile,
		"--no-metrics",
	}
	if allowConnectionsWithoutCertificates {
		args = append(args, "--tlsAllowConnectionsWithoutCertificates")
	}
	if keyPassword != "" {
		args = append(args, "--tlsCertificateKeyFilePassword", keyPassword)
	}
	if disabledProtocols != "" {
		args = append(args, "--tlsDisabledProtocols", disabledProtocols)
	}
	cmd := exec.Command(binary, args...)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 15*time.Second, 100*time.Millisecond)
	return addr
}

func newTLSClient(t *testing.T, addr string, roots *x509.CertPool) *mongo.Client {
	t.Helper()
	client, err := mongo.Connect(options.Client().
		ApplyURI("mongodb://" + addr + "/?directConnection=true").
		SetTLSConfig(&tls.Config{RootCAs: roots}).
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
	require.NoError(t, err)
	return client
}

func writeTLSCertificates(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "DumboDB test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	require.NoError(t, err)
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	require.NoError(t, err)

	certificateKeyFile := filepath.Join(t.TempDir(), "server.pem")
	certificateKeyPEM := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER})...,
	)
	require.NoError(t, os.WriteFile(certificateKeyFile, certificateKeyPEM, 0o600))

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	require.NoError(t, os.WriteFile(caFile, caPEM, 0o600))
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(caPEM))
	return certificateKeyFile, caFile, roots
}

func encryptTLSPrivateKey(t *testing.T, certificateKeyFile, password string) string {
	t.Helper()
	contents, err := os.ReadFile(certificateKeyFile)
	require.NoError(t, err)
	certificateBlock, rest := pem.Decode(contents)
	require.NotNil(t, certificateBlock)
	keyBlock, _ := pem.Decode(rest)
	require.NotNil(t, keyBlock)
	privateKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	require.NoError(t, err)
	encryptedDER, err := pkcs8.MarshalPrivateKey(privateKey, []byte(password), nil)
	require.NoError(t, err)
	encryptedContents := append(
		pem.EncodeToMemory(certificateBlock),
		pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: encryptedDER})...,
	)
	path := filepath.Join(t.TempDir(), "server-encrypted.pem")
	require.NoError(t, os.WriteFile(path, encryptedContents, 0o600))
	return path
}
