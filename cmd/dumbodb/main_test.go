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

package main

import (
	"flag"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRejectUnsupportedTLSFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		message string
	}{
		{name: "none"},
		{name: "certificate password", args: []string{"--tlsCertificateKeyFilePassword", "secret"}, message: "--tlsCertificateKeyFilePassword is a MongoDB TLS option"},
		{name: "disabled protocols", args: []string{"--tlsDisabledProtocols", "TLS1_0"}, message: "--tlsDisabledProtocols is a MongoDB TLS option"},
		{name: "invalid certificates", args: []string{"--tlsAllowInvalidCertificates"}, message: "--tlsAllowInvalidCertificates is a MongoDB TLS option"},
		{name: "invalid hostnames", args: []string{"--tlsAllowInvalidHostnames"}, message: "--tlsAllowInvalidHostnames is a MongoDB TLS option"},
		{name: "log versions", args: []string{"--tlsLogVersions", "TLS1_2"}, message: "--tlsLogVersions is a MongoDB TLS option"},
		{name: "normal ports", args: []string{"--tlsOnNormalPorts"}, message: "--tlsOnNormalPorts is a MongoDB TLS option"},
		{name: "cluster file", args: []string{"--tlsClusterFile", "cluster.pem"}, message: "--tlsClusterFile is a MongoDB replica-set TLS option"},
		{name: "cluster password", args: []string{"--tlsClusterPassword", "secret"}, message: "--tlsClusterPassword is a MongoDB replica-set TLS option"},
		{name: "cluster CA", args: []string{"--tlsClusterCAFile", "ca.pem"}, message: "--tlsClusterCAFile is a MongoDB replica-set TLS option"},
		{name: "cluster extension", args: []string{"--tlsClusterAuthX509ExtensionValue", "value"}, message: "--tlsClusterAuthX509ExtensionValue is a MongoDB replica-set TLS option"},
		{name: "cluster attributes", args: []string{"--tlsClusterAuthX509Attributes", "O=example"}, message: "--tlsClusterAuthX509Attributes is a MongoDB replica-set TLS option"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			registerUnsupportedTLSFlags(fs)
			assert.NoError(t, fs.Parse(test.args))
			err := rejectUnsupportedTLSFlags(fs)
			if test.message == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, test.message)
		})
	}
}

func TestValidateTLSFlags(t *testing.T) {
	tests := []struct {
		name                                string
		mode                                string
		certificateKeyFile                  string
		caFile                              string
		crlFile                             string
		allowConnectionsWithoutCertificates bool
		wantError                           string
	}{
		{name: "disabled", mode: "disabled"},
		{name: "certificate while disabled", mode: "disabled", certificateKeyFile: "server.pem", wantError: "need to enable TLS"},
		{name: "CRL while disabled", mode: "disabled", crlFile: "revocations.pem", wantError: "need to enable TLS"},
		{name: "invalid mode", mode: "sometimesTLS", wantError: "invalid --tlsMode"},
		{name: "allow TLS unsupported", mode: "allowTLS", wantError: "is not supported"},
		{name: "prefer TLS unsupported", mode: "preferTLS", wantError: "is not supported"},
		{name: "certificate required", mode: "requireTLS", wantError: "--tlsCertificateKeyFile is required"},
		{name: "CA required", mode: "requireTLS", certificateKeyFile: "server.pem", wantError: "--tlsCAFile is required"},
		{name: "mutual TLS", mode: "requireTLS", certificateKeyFile: "server.pem", caFile: "ca.pem"},
		{
			name:                                "optional client certificate",
			mode:                                "requireTLS",
			certificateKeyFile:                  "server.pem",
			caFile:                              "ca.pem",
			allowConnectionsWithoutCertificates: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTLSFlags(test.mode, test.certificateKeyFile, test.caFile, test.crlFile, test.allowConnectionsWithoutCertificates)
			if test.wantError == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestReplicationControlConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		replSet string
		wantOn  bool
	}{
		{name: "disabled"},
		{name: "enabled", replSet: "rs0", wantOn: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configuration, enabled := replicationControlConfiguration(test.replSet, "dumbo.example:27017")
			if enabled != test.wantOn {
				t.Fatalf("enabled = %v, want %v", enabled, test.wantOn)
			}
			if enabled && (configuration.SetName != test.replSet || configuration.MemberHost != "dumbo.example:27017") {
				t.Fatalf("configuration = %+v", configuration)
			}
		})
	}
}
