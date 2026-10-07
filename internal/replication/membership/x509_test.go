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
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"testing"
)

func TestX509PolicyDefaultAttributes(t *testing.T) {
	local := certificateWithNames(
		pkix.AttributeTypeAndValue{Type: distinguishedNameOIDs["O"], Value: "Example"},
		pkix.AttributeTypeAndValue{Type: distinguishedNameOIDs["OU"], Value: "Database"},
		pkix.AttributeTypeAndValue{Type: distinguishedNameOIDs["CN"], Value: "dumbo"},
	)
	policy, err := NewX509Policy(local, "", "")
	if err != nil {
		t.Fatal(err)
	}
	peer := certificateWithNames(
		pkix.AttributeTypeAndValue{Type: distinguishedNameOIDs["O"], Value: "example"},
		pkix.AttributeTypeAndValue{Type: distinguishedNameOIDs["OU"], Value: "Database"},
		pkix.AttributeTypeAndValue{Type: distinguishedNameOIDs["CN"], Value: "mongo"},
	)
	if !policy.Matches(peer) {
		t.Fatal("policy rejected peer with matching O and OU")
	}
	peer.Subject.Names[1].Value = "Other"
	if policy.Matches(peer) {
		t.Fatal("policy accepted peer with a different OU")
	}
}

func TestX509PolicyConfiguredExtension(t *testing.T) {
	encoded, err := asn1.Marshal("replica-cluster")
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{Extensions: []pkix.Extension{{Id: clusterMembershipOID, Value: encoded}}}
	policy, err := NewX509Policy(certificate, "", "replica-cluster")
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Matches(certificate) {
		t.Fatal("policy rejected matching membership extension")
	}
}

func TestParseAuthMode(t *testing.T) {
	for _, value := range []string{"keyFile", "sendKeyFile", "sendX509", "x509"} {
		if _, err := ParseAuthMode(value); err != nil {
			t.Fatalf("ParseAuthMode(%q): %v", value, err)
		}
	}
	if _, err := ParseAuthMode("tls"); err == nil {
		t.Fatal("ParseAuthMode accepted an unknown mode")
	}
}

func certificateWithNames(names ...pkix.AttributeTypeAndValue) *x509.Certificate {
	return &x509.Certificate{Subject: pkix.Name{Names: names}}
}
