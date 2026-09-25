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

package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/youmark/pkcs8"
)

func TestServerConfigRejectsExpiredCertificate(t *testing.T) {
	certificateKeyFile := writeCertificateKeyFile(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	_, err := ServerConfig(ServerConfigOptions{CertificateFile: certificateKeyFile, KeyFile: certificateKeyFile})
	require.ErrorContains(t, err, "certificate expired")
}

func TestServerConfigRejectsCertificateBeforeValidityWindow(t *testing.T) {
	certificateKeyFile := writeCertificateKeyFile(t, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	_, err := ServerConfig(ServerConfigOptions{CertificateFile: certificateKeyFile, KeyFile: certificateKeyFile})
	require.ErrorContains(t, err, "certificate is not valid before")
}

func TestServerConfigDecryptsPKCS8PrivateKey(t *testing.T) {
	path := writeEncryptedCertificateKeyFile(t, "correct-password")
	_, err := ServerConfig(ServerConfigOptions{
		CertificateFile: path,
		KeyFile:         path,
		KeyPassword:     "correct-password",
	})
	require.NoError(t, err)

	_, err = ServerConfig(ServerConfigOptions{CertificateFile: path, KeyFile: path})
	require.ErrorContains(t, err, "is encrypted")
	require.ErrorContains(t, err, "--tlsCertificateKeyFilePassword")

	_, err = ServerConfig(ServerConfigOptions{
		CertificateFile: path,
		KeyFile:         path,
		KeyPassword:     "wrong-password",
	})
	require.ErrorContains(t, err, "incorrect password")
}

func TestServerConfigRejectsLegacyEncryptedPrivateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.pem")
	block := &pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: map[string]string{"DEK-Info": "AES-256-CBC,0123456789ABCDEF"},
		Bytes:   []byte("encrypted"),
	}
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0o600))

	_, err := ServerConfig(ServerConfigOptions{
		CertificateFile: path,
		KeyFile:         path,
		KeyPassword:     "password",
	})
	require.ErrorContains(t, err, "legacy PEM encryption")
	require.ErrorContains(t, err, "encrypted PKCS#8")
}

func TestVerifyChainRevocation(t *testing.T) {
	issuer, issuerKey, certificate := certificateChain(t)
	now := time.Now()
	tests := []struct {
		name      string
		lists     []*x509.RevocationList
		wantError string
	}{
		{
			name:  "not revoked",
			lists: []*x509.RevocationList{revocationList(t, issuer, issuerKey, nil, now.Add(-time.Hour), now.Add(time.Hour))},
		},
		{
			name: "revoked",
			lists: []*x509.RevocationList{revocationList(t, issuer, issuerKey, []x509.RevocationListEntry{
				{SerialNumber: certificate.SerialNumber, RevocationTime: now.Add(-time.Minute)},
			}, now.Add(-time.Hour), now.Add(time.Hour))},
			wantError: "is revoked",
		},
		{
			name:      "expired",
			lists:     []*x509.RevocationList{revocationList(t, issuer, issuerKey, nil, now.Add(-2*time.Hour), now.Add(-time.Hour))},
			wantError: "CRL expired",
		},
		{
			name:      "not yet valid",
			lists:     []*x509.RevocationList{revocationList(t, issuer, issuerKey, nil, now.Add(time.Hour), now.Add(2*time.Hour))},
			wantError: "CRL is not valid before",
		},
		{
			name: "valid list supersedes expired list",
			lists: []*x509.RevocationList{
				revocationList(t, issuer, issuerKey, nil, now.Add(-2*time.Hour), now.Add(-time.Hour)),
				revocationList(t, issuer, issuerKey, nil, now.Add(-time.Hour), now.Add(time.Hour)),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyChainRevocation([]*x509.Certificate{certificate, issuer}, test.lists, now)
			if test.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestServerConfigRejectsRevokedClientCertificate(t *testing.T) {
	now := time.Now()
	issuer, issuerKey, clientCertificate := certificateChain(t)
	serverFile := writeCertificateKeyFile(t, now.Add(-time.Hour), now.Add(time.Hour))
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: issuer.Raw,
	}), 0o600))
	list := revocationList(t, issuer, issuerKey, []x509.RevocationListEntry{
		{SerialNumber: clientCertificate.SerialNumber, RevocationTime: now.Add(-time.Minute)},
	}, now.Add(-time.Hour), now.Add(time.Hour))
	crlFile := filepath.Join(t.TempDir(), "revocations.pem")
	require.NoError(t, os.WriteFile(crlFile, pem.EncodeToMemory(&pem.Block{
		Type: "X509 CRL", Bytes: list.Raw,
	}), 0o600))

	config, err := ServerConfig(ServerConfigOptions{
		CertificateFile: serverFile,
		KeyFile:         serverFile,
		CAFile:          caFile,
		CRLFile:         crlFile,
	})
	require.NoError(t, err)
	require.NotNil(t, config.VerifyConnection)
	err = config.VerifyConnection(tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{clientCertificate, issuer}},
	})
	require.ErrorContains(t, err, "is revoked")
}

func TestServerConfigSelectsEnabledProtocol(t *testing.T) {
	now := time.Now()
	serverFile := writeCertificateKeyFile(t, now.Add(-time.Hour), now.Add(time.Hour))
	defaultConfig, err := ServerConfig(ServerConfigOptions{
		CertificateFile: serverFile,
		KeyFile:         serverFile,
	})
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS12), defaultConfig.MinVersion)
	require.Equal(t, uint16(tls.VersionTLS13), defaultConfig.MaxVersion)
	require.Nil(t, defaultConfig.GetConfigForClient)

	tests := []struct {
		name      string
		disabled  []uint16
		supported []uint16
		want      uint16
		wantError string
	}{
		{name: "TLS 1.3 disabled", disabled: []uint16{tls.VersionTLS13}, supported: []uint16{tls.VersionTLS13, tls.VersionTLS12}, want: tls.VersionTLS12},
		{name: "TLS 1.2 disabled", disabled: []uint16{tls.VersionTLS12}, supported: []uint16{tls.VersionTLS12, tls.VersionTLS11, tls.VersionTLS13}, want: tls.VersionTLS13},
		{name: "unordered client versions", disabled: []uint16{tls.VersionTLS11}, supported: []uint16{tls.VersionTLS12, tls.VersionTLS11, tls.VersionTLS13}, want: tls.VersionTLS13},
		{name: "legacy remains disabled", disabled: []uint16{tls.VersionTLS10}, supported: []uint16{tls.VersionTLS11, tls.VersionTLS10}, wantError: "does not support an enabled TLS protocol version"},
		{
			name:      "all enabled protocols disabled",
			disabled:  []uint16{tls.VersionTLS12, tls.VersionTLS13},
			supported: []uint16{tls.VersionTLS13, tls.VersionTLS12},
			wantError: "does not support an enabled TLS protocol version",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := ServerConfig(ServerConfigOptions{
				CertificateFile:   serverFile,
				KeyFile:           serverFile,
				DisabledProtocols: test.disabled,
			})
			require.NoError(t, err)
			selected, err := config.GetConfigForClient(&tls.ClientHelloInfo{SupportedVersions: test.supported})
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, selected.MinVersion)
			require.Equal(t, test.want, selected.MaxVersion)
		})
	}
}

func TestOptionalTLSConn(t *testing.T) {
	now := time.Now()
	serverFile := writeCertificateKeyFile(t, now.Add(-time.Hour), now.Add(time.Hour))
	config, err := ServerConfig(ServerConfigOptions{
		CertificateFile: serverFile,
		KeyFile:         serverFile,
	})
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(config.Certificates[0].Certificate[0])
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)

	tests := []struct {
		name    string
		useTLS  bool
		payload []byte
	}{
		{name: "plaintext", payload: []byte("plaintext")},
		{name: "plaintext starts with TLS content type", payload: []byte{0x16, 0x00, 0x00, 0x00, 'x'}},
		{name: "TLS", useTLS: true, payload: []byte("encrypted")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()
			require.NoError(t, serverConn.SetDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, clientConn.SetDeadline(time.Now().Add(5*time.Second)))

			var client net.Conn = clientConn
			if test.useTLS {
				client = tls.Client(clientConn, &tls.Config{
					MinVersion: tls.VersionTLS12,
					RootCAs:    roots,
					ServerName: "localhost",
				})
			}
			writeResult := make(chan error, 1)
			go func() {
				_, err := client.Write(test.payload)
				writeResult <- err
			}()

			optional := &optionalTLSConn{Conn: serverConn, config: config}
			received := make([]byte, len(test.payload))
			_, err = io.ReadFull(optional, received)
			require.NoError(t, err)
			require.Equal(t, test.payload, received)
			require.NoError(t, <-writeResult)
		})
	}
}

func certificateChain(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	now := time.Now()
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	issuerTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTemplate, issuerTemplate, &issuerKey.PublicKey, issuerKey)
	require.NoError(t, err)
	issuer, err := x509.ParseCertificate(issuerDER)
	require.NoError(t, err)

	certificateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	certificateTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, certificateTemplate, issuer, &certificateKey.PublicKey, issuerKey)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(certificateDER)
	require.NoError(t, err)
	return issuer, issuerKey, certificate
}

func revocationList(
	t *testing.T,
	issuer *x509.Certificate,
	issuerKey *ecdsa.PrivateKey,
	entries []x509.RevocationListEntry,
	thisUpdate time.Time,
	nextUpdate time.Time,
) *x509.RevocationList {
	t.Helper()
	contents, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:                    big.NewInt(1),
		ThisUpdate:                thisUpdate,
		NextUpdate:                nextUpdate,
		RevokedCertificateEntries: entries,
	}, issuer, issuerKey)
	require.NoError(t, err)
	list, err := x509.ParseRevocationList(contents)
	require.NoError(t, err)
	return list
}

func writeCertificateKeyFile(t *testing.T, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	contents := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...,
	)
	path := filepath.Join(t.TempDir(), "server.pem")
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}

func writeEncryptedCertificateKeyFile(t *testing.T, password string) string {
	t.Helper()
	path := writeCertificateKeyFile(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	contents, err := os.ReadFile(path)
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
	require.NoError(t, os.WriteFile(path, encryptedContents, 0o600))
	return path
}
