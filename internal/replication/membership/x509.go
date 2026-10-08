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
	"encoding/asn1"
	"fmt"
	"strings"
	"sync/atomic"
)

type AuthMode string

const (
	AuthModeKeyFile     AuthMode = "keyFile"
	AuthModeSendKeyFile AuthMode = "sendKeyFile"
	AuthModeSendX509    AuthMode = "sendX509"
	AuthModeX509        AuthMode = "x509"
)

func ParseAuthMode(value string) (AuthMode, error) {
	mode := AuthMode(value)
	switch mode {
	case AuthModeKeyFile, AuthModeSendKeyFile, AuthModeSendX509, AuthModeX509:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid --clusterAuthMode %q", value)
	}
}

func (m AuthMode) AllowsKeyFile() bool {
	return m == AuthModeKeyFile || m == AuthModeSendKeyFile || m == AuthModeSendX509
}

func (m AuthMode) SendsKeyFile() bool {
	return m == AuthModeKeyFile || m == AuthModeSendKeyFile
}

func (m AuthMode) AllowsX509() bool {
	return m == AuthModeSendKeyFile || m == AuthModeSendX509 || m == AuthModeX509
}

func (m AuthMode) SendsX509() bool {
	return m == AuthModeSendX509 || m == AuthModeX509
}

var clusterMembershipOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 34601, 2, 1, 2}

var distinguishedNameOIDs = map[string]asn1.ObjectIdentifier{
	"C":  {2, 5, 4, 6},
	"CN": {2, 5, 4, 3},
	"DC": {0, 9, 2342, 19200300, 100, 1, 25},
	"L":  {2, 5, 4, 7},
	"O":  {2, 5, 4, 10},
	"OU": {2, 5, 4, 11},
	"ST": {2, 5, 4, 8},
}

type x509Attribute struct {
	oid   asn1.ObjectIdentifier
	value string
}

type X509Policy struct {
	criteria atomic.Pointer[x509Criteria]
}

type x509Criteria struct {
	attributes     []x509Attribute
	extensionValue string
}

func NewX509Policy(localCertificate *x509.Certificate, attributes, extensionValue string) (*X509Policy, error) {
	if attributes != "" && extensionValue != "" {
		return nil, fmt.Errorf("--tlsClusterAuthX509Attributes and --tlsClusterAuthX509ExtensionValue are mutually exclusive")
	}
	criteria := &x509Criteria{extensionValue: extensionValue}
	var err error
	if attributes != "" {
		criteria.attributes, err = parseAttributes(attributes)
		if err != nil {
			return nil, fmt.Errorf("invalid --tlsClusterAuthX509Attributes: %w", err)
		}
	} else if extensionValue == "" {
		for _, name := range localCertificate.Subject.Names {
			if !isDefaultClusterAttribute(name.Type) {
				continue
			}
			value, ok := name.Value.(string)
			if ok {
				criteria.attributes = append(criteria.attributes, x509Attribute{oid: name.Type, value: value})
			}
		}
		if len(criteria.attributes) == 0 {
			return nil, fmt.Errorf("local TLS certificate subject must contain at least one O, OU, or DC attribute")
		}
	}
	if len(criteria.attributes) != 0 && !criteria.matchesAttributes(localCertificate) {
		return nil, fmt.Errorf("local TLS certificate subject does not contain the configured cluster authentication attributes")
	}
	if extensionValue != "" && !matchesExtension(localCertificate, extensionValue) {
		return nil, fmt.Errorf("local TLS certificate does not contain the configured cluster membership extension")
	}
	policy := &X509Policy{}
	policy.criteria.Store(criteria)
	return policy, nil
}

func (p *X509Policy) Matches(certificate *x509.Certificate) bool {
	if p == nil || certificate == nil {
		return false
	}
	criteria := p.criteria.Load()
	if criteria == nil {
		return false
	}
	if criteria.extensionValue != "" {
		return matchesExtension(certificate, criteria.extensionValue)
	}
	return criteria.matchesAttributes(certificate)
}

func (p *X509Policy) Replace(replacement *X509Policy) {
	if p != nil && replacement != nil {
		p.criteria.Store(replacement.criteria.Load())
	}
}

func (p *x509Criteria) matchesAttributes(certificate *x509.Certificate) bool {
	for _, required := range p.attributes {
		matched := false
		for _, actual := range certificate.Subject.Names {
			value, ok := actual.Value.(string)
			if ok && actual.Type.Equal(required.oid) && strings.EqualFold(value, required.value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return len(p.attributes) != 0
}

func matchesExtension(certificate *x509.Certificate, expected string) bool {
	for _, extension := range certificate.Extensions {
		if !extension.Id.Equal(clusterMembershipOID) {
			continue
		}
		var value string
		if rest, err := asn1.Unmarshal(extension.Value, &value); err == nil && len(rest) == 0 {
			return value == expected
		}
	}
	return false
}

func parseAttributes(value string) ([]x509Attribute, error) {
	parts, err := splitDistinguishedName(value)
	if err != nil {
		return nil, err
	}
	attributes := make([]x509Attribute, 0, len(parts))
	for _, part := range parts {
		key, attributeValue, found := strings.Cut(part, "=")
		if !found || strings.TrimSpace(attributeValue) == "" {
			return nil, fmt.Errorf("attribute %q must have a value", part)
		}
		oid, ok := distinguishedNameOIDs[strings.ToUpper(strings.TrimSpace(key))]
		if !ok {
			return nil, fmt.Errorf("unsupported distinguished-name attribute %q", key)
		}
		attributes = append(attributes, x509Attribute{oid: oid, value: unescapeAttribute(strings.TrimSpace(attributeValue))})
	}
	if len(attributes) == 0 {
		return nil, fmt.Errorf("no attributes were configured")
	}
	return attributes, nil
}

func splitDistinguishedName(value string) ([]string, error) {
	var parts []string
	start := 0
	escaped := false
	for index := range len(value) {
		switch value[index] {
		case '\\':
			escaped = !escaped
		case ',':
			if !escaped {
				parts = append(parts, strings.TrimSpace(value[start:index]))
				start = index + 1
			}
			escaped = false
		default:
			escaped = false
		}
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape")
	}
	parts = append(parts, strings.TrimSpace(value[start:]))
	return parts, nil
}

func unescapeAttribute(value string) string {
	value = strings.ReplaceAll(value, "\\,", ",")
	value = strings.ReplaceAll(value, "\\=", "=")
	return strings.ReplaceAll(value, "\\\\", "\\")
}

func isDefaultClusterAttribute(oid asn1.ObjectIdentifier) bool {
	return oid.Equal(distinguishedNameOIDs["O"]) || oid.Equal(distinguishedNameOIDs["OU"]) || oid.Equal(distinguishedNameOIDs["DC"])
}
