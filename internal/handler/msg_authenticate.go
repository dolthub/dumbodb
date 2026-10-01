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

package handler

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgAuthenticate(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document, "mechanism", "user"); err != nil {
		return nil, err
	}

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	mechanism, err := common.GetRequiredParam[string](document, "mechanism")
	if err != nil {
		return nil, err
	}
	if mechanism != "MONGODB-X509" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrMechanismUnavailable,
			fmt.Sprintf("Received authentication for mechanism %s which is not enabled", mechanism),
			"mechanism",
		)
	}
	if dbName != "$external" {
		return nil, authenticationFailed("MONGODB-X509 authentication must use the $external database")
	}

	ci := conninfo.Get(connCtx)
	if ci.Authenticated() {
		return nil, authenticationFailed("Authentication failed.")
	}
	if !ci.UsesTLS() {
		return nil, authenticationFailed("MONGODB-X509 authentication requires a TLS connection")
	}

	certificate := ci.PeerCertificate()
	if certificate == nil {
		return nil, authenticationFailed("MONGODB-X509 authentication requires a verified client certificate")
	}

	subject, err := x509Subject(certificate)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	claimedUser, err := common.GetOptionalParam(document, "user", "")
	if err != nil {
		return nil, err
	}
	if claimedUser != "" && claimedUser != subject {
		return nil, authenticationFailed("Authentication failed.")
	}

	user, err := h.loadUserDoc(connCtx, "$external", subject)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrUserNotFound,
			fmt.Sprintf("Could not find user %q for db %q", subject, "$external"),
		)
	}

	if err = h.checkAuthRestrictions(connCtx, "$external", subject); err != nil {
		return nil, err
	}

	ci.SetAuth(subject, "", nil, "$external")
	ci.SetBypassBackendAuth()
	ci.SetAuthenticated()

	return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
}

func x509Subject(certificate *x509.Certificate) (string, error) {
	var subject pkix.RDNSequence
	rest, err := asn1.Unmarshal(certificate.RawSubject, &subject)
	if err != nil {
		return "", err
	}
	if len(rest) != 0 {
		return "", fmt.Errorf("certificate subject contains trailing ASN.1 data")
	}
	return escapeRFC2253NonASCII(subject.String()), nil
}

func escapeRFC2253NonASCII(subject string) string {
	const uppercaseHex = "0123456789ABCDEF"

	var escaped strings.Builder
	escaped.Grow(len(subject))
	for index := 0; index < len(subject); index++ {
		value := subject[index]
		if value < utf8.RuneSelf {
			escaped.WriteByte(value)
			continue
		}
		escaped.WriteByte('\\')
		escaped.WriteByte(uppercaseHex[value>>4])
		escaped.WriteByte(uppercaseHex[value&0x0f])
	}
	return escaped.String()
}

func authenticationFailed(message string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrAuthenticationFailed,
		message,
		"authenticate",
	)
}
