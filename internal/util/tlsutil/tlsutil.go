// Copyright 2021 FerretDB Inc.
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

// Package tlsutil provides TLS utilities.
package tlsutil

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/youmark/pkcs8"
)

type ServerConfigOptions struct {
	CertificateFile                     string
	KeyFile                             string
	KeyPassword                         string
	CAFile                              string
	CRLFile                             string
	AllowConnectionsWithoutCertificates bool
	DisabledProtocols                   []uint16
}

type optionalTLSListener struct {
	net.Listener
	config *tls.Config
}

type optionalTLSConn struct {
	net.Conn
	config   *tls.Config
	once     sync.Once
	selected net.Conn
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func ServerConfig(opts ServerConfigOptions) (*tls.Config, error) {
	config, ca, err := config(opts.CertificateFile, opts.KeyFile, opts.KeyPassword, opts.CAFile)
	if err != nil {
		return nil, err
	}
	if ca != nil {
		config.ClientAuth = tls.RequireAndVerifyClientCert
		if opts.AllowConnectionsWithoutCertificates {
			config.ClientAuth = tls.VerifyClientCertIfGiven
		}
		config.ClientCAs = ca
	}
	if opts.CRLFile != "" {
		if ca == nil {
			return nil, fmt.Errorf("TLS CRL file requires a CA file")
		}
		revocationLists, err := loadRevocationLists(opts.CRLFile)
		if err != nil {
			return nil, err
		}
		config.VerifyConnection = func(state tls.ConnectionState) error {
			return verifyRevocation(state.VerifiedChains, revocationLists, time.Now())
		}
	}
	config.MinVersion = tls.VersionTLS12
	config.MaxVersion = tls.VersionTLS13
	if len(opts.DisabledProtocols) > 0 {
		disabled := make(map[uint16]struct{}, len(opts.DisabledProtocols))
		for _, version := range opts.DisabledProtocols {
			disabled[version] = struct{}{}
		}
		config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			var highestEnabled uint16
			for _, version := range hello.SupportedVersions {
				if _, found := disabled[version]; found {
					continue
				}
				if version >= config.MinVersion && version <= config.MaxVersion && version > highestEnabled {
					highestEnabled = version
				}
			}
			if highestEnabled != 0 {
				selected := config.Clone()
				selected.GetConfigForClient = nil
				selected.MinVersion = highestEnabled
				selected.MaxVersion = highestEnabled
				return selected, nil
			}
			return nil, fmt.Errorf("client does not support an enabled TLS protocol version")
		}
	}
	return config, nil
}

// OptionalListener accepts both TLS and plaintext connections.
func OptionalListener(listener net.Listener, config *tls.Config) net.Listener {
	return &optionalTLSListener{Listener: listener, config: config}
}

// PeerCertificate returns the verified leaf certificate presented by the client.
func PeerCertificate(ctx context.Context, conn net.Conn) (*x509.Certificate, error) {
	if optional, ok := conn.(*optionalTLSConn); ok {
		optional.selectProtocol()
		conn = optional.selected
	}
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return nil, nil
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	verifiedChains := tlsConn.ConnectionState().VerifiedChains
	if len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
		return nil, nil
	}
	return verifiedChains[0][0], nil
}

func (l *optionalTLSListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &optionalTLSConn{Conn: conn, config: l.config}, nil
}

func (c *optionalTLSConn) Read(p []byte) (int, error) {
	c.selectProtocol()
	return c.selected.Read(p)
}

func (c *optionalTLSConn) Write(p []byte) (int, error) {
	c.selectProtocol()
	return c.selected.Write(p)
}

func (c *optionalTLSConn) selectProtocol() {
	c.once.Do(func() {
		reader := bufio.NewReader(c.Conn)
		prefix, _ := reader.Peek(3)
		var selected net.Conn = &bufferedConn{Conn: c.Conn, reader: reader}
		if len(prefix) == 3 && prefix[0] == 0x16 && prefix[1] == 0x03 && prefix[2] >= 0x01 && prefix[2] <= 0x04 {
			selected = tls.Server(selected, c.config)
		}
		c.selected = selected
	})
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func ClientConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	config, ca, err := config(certFile, keyFile, "", caFile)
	if err != nil {
		return nil, err
	}
	config.RootCAs = ca
	return config, nil
}

func config(certFile, keyFile, keyPassword, caFile string) (*tls.Config, *x509.CertPool, error) {
	if _, err := os.Stat(certFile); err != nil {
		return nil, nil, fmt.Errorf("TLS certificate file: %w", err)
	}
	certificateContents, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, fmt.Errorf("TLS certificate file: %w", err)
	}

	if _, err := os.Stat(keyFile); err != nil {
		return nil, nil, fmt.Errorf("TLS key file: %w", err)
	}
	keyContents, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("TLS key file: %w", err)
	}
	keyContents, err = decryptPrivateKeys(keyFile, keyContents, keyPassword)
	if err != nil {
		return nil, nil, err
	}

	cert, err := tls.X509KeyPair(certificateContents, keyContents)
	if err != nil {
		return nil, nil, fmt.Errorf("TLS file pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("TLS certificate file %q: %w", certFile, err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) {
		return nil, nil, fmt.Errorf("TLS certificate file %q: certificate is not valid before %s", certFile, leaf.NotBefore.Format(time.RFC3339))
	}
	if now.After(leaf.NotAfter) {
		return nil, nil, fmt.Errorf("TLS certificate file %q: certificate expired at %s", certFile, leaf.NotAfter.Format(time.RFC3339))
	}

	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	if caFile == "" {
		return config, nil, nil
	}

	if _, err := os.Stat(caFile); err != nil {
		return nil, nil, fmt.Errorf("TLS CA file: %w", err)
	}
	b, err := os.ReadFile(caFile)
	if err != nil {
		return nil, nil, err
	}
	ca := x509.NewCertPool()
	if ok := ca.AppendCertsFromPEM(b); !ok {
		return nil, nil, fmt.Errorf("TLS CA file: failed to parse")
	}
	return config, ca, nil
}

func decryptPrivateKeys(path string, contents []byte, password string) ([]byte, error) {
	original := contents
	var blocks []*pem.Block
	var decrypted bool
	for {
		block, rest := pem.Decode(contents)
		if block == nil {
			break
		}
		if strings.HasSuffix(block.Type, "PRIVATE KEY") && block.Headers["DEK-Info"] != "" {
			return nil, fmt.Errorf("TLS key file %q uses legacy PEM encryption, which DumboDB does not support; convert it to encrypted PKCS#8 with 'openssl pkcs8 -topk8 -in legacy-key.pem -out key.pem'", path)
		}
		if block.Type == "ENCRYPTED PRIVATE KEY" {
			if password == "" {
				return nil, fmt.Errorf("TLS key file %q is encrypted; provide its password with --tlsCertificateKeyFilePassword", path)
			}
			privateKey, err := pkcs8.ParsePKCS8PrivateKey(block.Bytes, []byte(password))
			if err != nil {
				return nil, fmt.Errorf("TLS key file %q: decrypting encrypted PKCS#8 private key: %w", path, err)
			}
			der, err := x509.MarshalPKCS8PrivateKey(privateKey)
			if err != nil {
				return nil, fmt.Errorf("TLS key file %q: encoding decrypted PKCS#8 private key: %w", path, err)
			}
			block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
			decrypted = true
		}
		blocks = append(blocks, block)
		contents = rest
	}
	if !decrypted {
		return original, nil
	}
	var result []byte
	for _, block := range blocks {
		result = append(result, pem.EncodeToMemory(block)...)
	}
	return result, nil
}

func loadRevocationLists(path string) ([]*x509.RevocationList, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("TLS CRL file: %w", err)
	}
	var lists []*x509.RevocationList
	rest := contents
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining
		if block.Type != "X509 CRL" {
			continue
		}
		list, err := x509.ParseRevocationList(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("TLS CRL file %q: %w", path, err)
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 {
		list, err := x509.ParseRevocationList(contents)
		if err != nil {
			return nil, fmt.Errorf("TLS CRL file %q: %w", path, err)
		}
		lists = append(lists, list)
	}
	return lists, nil
}

func verifyRevocation(chains [][]*x509.Certificate, lists []*x509.RevocationList, now time.Time) error {
	if len(chains) == 0 {
		return nil
	}
	var firstError error
	for _, chain := range chains {
		err := verifyChainRevocation(chain, lists, now)
		if err == nil {
			return nil
		}
		if firstError == nil {
			firstError = err
		}
	}
	return firstError
}

func verifyChainRevocation(chain []*x509.Certificate, lists []*x509.RevocationList, now time.Time) error {
	for i := 0; i+1 < len(chain); i++ {
		certificate := chain[i]
		issuer := chain[i+1]
		var validLists []*x509.RevocationList
		var listError error
		for _, list := range lists {
			if !bytes.Equal(list.RawIssuer, certificate.RawIssuer) {
				continue
			}
			if err := list.CheckSignatureFrom(issuer); err != nil {
				listError = fmt.Errorf("CRL signature validation failed: %w", err)
				continue
			}
			if now.Before(list.ThisUpdate) {
				listError = fmt.Errorf("CRL is not valid before %s", list.ThisUpdate.Format(time.RFC3339))
				continue
			}
			if list.NextUpdate.IsZero() || now.After(list.NextUpdate) {
				listError = fmt.Errorf("CRL expired at %s", list.NextUpdate.Format(time.RFC3339))
				continue
			}
			validLists = append(validLists, list)
		}
		if len(validLists) == 0 {
			if listError != nil {
				return listError
			}
			return fmt.Errorf("no CRL found for certificate issuer %q", certificate.Issuer.String())
		}
		for _, list := range validLists {
			for _, entry := range list.RevokedCertificateEntries {
				if certificate.SerialNumber.Cmp(entry.SerialNumber) == 0 {
					return fmt.Errorf("certificate with serial number %s is revoked", certificate.SerialNumber)
				}
			}
		}
	}
	return nil
}
