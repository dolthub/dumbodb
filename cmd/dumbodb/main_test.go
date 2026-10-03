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
	"crypto/tls"
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
		{name: "invalid certificates", args: []string{"--tlsAllowInvalidCertificates"}, message: "--tlsAllowInvalidCertificates is a MongoDB replica-set TLS option"},
		{name: "invalid hostnames", args: []string{"--tlsAllowInvalidHostnames"}, message: "--tlsAllowInvalidHostnames is a MongoDB replica-set TLS option"},
		{name: "log versions", args: []string{"--tlsLogVersions", "TLS1_2"}, message: "--tlsLogVersions is a MongoDB TLS option"},
		{name: "normal ports", args: []string{"--tlsOnNormalPorts"}, message: "--tlsOnNormalPorts is a MongoDB TLS option"},
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

func TestValidateClusterTLSFlags(t *testing.T) {
	tests := []struct {
		name            string
		mode            string
		replSet         string
		clusterFile     string
		clusterPassword string
		clusterCA       string
		wantError       string
	}{
		{name: "not configured", mode: "disabled"},
		{name: "cluster file", mode: "requireTLS", replSet: "rs0", clusterFile: "cluster.pem"},
		{name: "no replica set", mode: "requireTLS", clusterFile: "cluster.pem", wantError: "require --replSet"},
		{name: "TLS disabled", mode: "disabled", replSet: "rs0", clusterFile: "cluster.pem", wantError: "require TLS"},
		{name: "password without file", mode: "requireTLS", replSet: "rs0", clusterPassword: "secret", wantError: "requires --tlsClusterFile"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateClusterTLSFlags(test.mode, test.replSet, test.clusterFile, test.clusterPassword, test.clusterCA)
			if test.wantError == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestValidateMembershipFlags(t *testing.T) {
	tests := []struct {
		name      string
		auth      bool
		replSet   string
		keyFile   string
		wantError string
	}{
		{name: "standalone auth", auth: true},
		{name: "unauthenticated replica set", replSet: "rs0"},
		{name: "authenticated replica set", auth: true, replSet: "rs0", keyFile: "keyfile"},
		{name: "key file implies auth", replSet: "rs0", keyFile: "keyfile"},
		{name: "missing key file", auth: true, replSet: "rs0", wantError: "--keyFile is required"},
		{name: "key file without replica set", keyFile: "keyfile", wantError: "--keyFile requires --replSet"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateMembershipFlags(test.auth, test.replSet, test.keyFile)
			if test.wantError == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestValidateTLSFlags(t *testing.T) {
	tests := []struct {
		name                                string
		mode                                string
		certificateKeyFile                  string
		certificateKeyFilePassword          string
		caFile                              string
		crlFile                             string
		disabledProtocolsSet                bool
		allowConnectionsWithoutCertificates bool
		wantError                           string
	}{
		{name: "disabled", mode: "disabled"},
		{name: "certificate while disabled", mode: "disabled", certificateKeyFile: "server.pem", wantError: "need to enable TLS"},
		{name: "certificate password while disabled", mode: "disabled", certificateKeyFilePassword: "secret", wantError: "need to enable TLS"},
		{name: "CRL while disabled", mode: "disabled", crlFile: "revocations.pem", wantError: "need to enable TLS"},
		{name: "disabled protocols while TLS disabled", mode: "disabled", disabledProtocolsSet: true, wantError: "need to enable TLS"},
		{name: "invalid mode", mode: "sometimesTLS", wantError: "invalid --tlsMode"},
		{name: "allow TLS certificate required", mode: "allowTLS", wantError: "--tlsCertificateKeyFile is required"},
		{name: "allow TLS", mode: "allowTLS", certificateKeyFile: "server.pem", caFile: "ca.pem"},
		{name: "prefer TLS certificate required", mode: "preferTLS", wantError: "--tlsCertificateKeyFile is required"},
		{name: "prefer TLS", mode: "preferTLS", certificateKeyFile: "server.pem", caFile: "ca.pem"},
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
			err := validateTLSFlags(test.mode, test.certificateKeyFile, test.certificateKeyFilePassword, test.caFile, test.crlFile, test.disabledProtocolsSet, test.allowConnectionsWithoutCertificates)
			if test.wantError == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestParseTLSDisabledProtocols(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		enabled   bool
		want      []uint16
		wantError string
	}{
		{name: "not set"},
		{name: "none", value: "none", enabled: true},
		{name: "single", value: "TLS1_2", enabled: true, want: []uint16{tls.VersionTLS12}},
		{name: "non-contiguous", value: "TLS1_1,TLS1_3", enabled: true, want: []uint16{tls.VersionTLS11, tls.VersionTLS13}},
		{name: "legacy names", value: "noTLS1_0,noTLS1_2", enabled: true, want: []uint16{tls.VersionTLS10, tls.VersionTLS12}},
		{name: "empty", enabled: true, wantError: "unrecognized"},
		{name: "unknown", value: "TLS2_0", enabled: true, wantError: "unrecognized"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseTLSDisabledProtocols(test.value, test.enabled)
			if test.wantError != "" {
				assert.ErrorContains(t, err, test.wantError)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, test.want, got)
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
