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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"
)

func ServerConfig(certFile, keyFile, caFile string, allowConnectionsWithoutCertificates bool) (*tls.Config, error) {
	config, ca, err := config(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}
	if ca != nil {
		config.ClientAuth = tls.RequireAndVerifyClientCert
		if allowConnectionsWithoutCertificates {
			config.ClientAuth = tls.VerifyClientCertIfGiven
		}
		config.ClientCAs = ca
	}
	return config, nil
}

func ClientConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	config, ca, err := config(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}
	config.RootCAs = ca
	return config, nil
}

func config(certFile, keyFile, caFile string) (*tls.Config, *x509.CertPool, error) {
	if _, err := os.Stat(certFile); err != nil {
		return nil, nil, fmt.Errorf("TLS certificate file: %w", err)
	}

	if _, err := os.Stat(keyFile); err != nil {
		return nil, nil, fmt.Errorf("TLS key file: %w", err)
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
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
