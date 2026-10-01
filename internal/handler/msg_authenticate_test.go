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
	"errors"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

var (
	organizationOID       = asn1.ObjectIdentifier{2, 5, 4, 10}
	organizationalUnitOID = asn1.ObjectIdentifier{2, 5, 4, 11}
	commonNameOID         = asn1.ObjectIdentifier{2, 5, 4, 3}
)

func testX509Certificate(t *testing.T) (*x509.Certificate, string) {
	t.Helper()

	raw := must.NotFail(asn1.Marshal(pkix.RDNSequence{
		{{Type: organizationOID, Value: "Example"}},
		{{Type: organizationalUnitOID, Value: "engineering"}},
		{{Type: commonNameOID, Value: "dumbo-client"}},
	}))

	return &x509.Certificate{RawSubject: raw}, "CN=dumbo-client,OU=engineering,O=Example"
}

func insertX509User(t *testing.T, h *Handler, subject string) {
	t.Helper()

	adminDB := must.NotFail(h.b.Database("admin"))
	users := must.NotFail(adminDB.Collection("system.users"))
	_, err := users.InsertAll(context.Background(), &backends.InsertAllParams{Docs: []*types.Document{
		must.NotFail(types.NewDocument(
			"_id", "$external."+subject,
			"user", subject,
			"db", "$external",
			"roles", types.MakeArray(0),
		)),
	}})
	require.NoError(t, err)
}

func authenticateMsg(t *testing.T, user *string) *wire.OpMsg {
	t.Helper()

	doc := must.NotFail(types.NewDocument(
		"authenticate", int32(1),
		"mechanism", "MONGODB-X509",
		"$db", "$external",
	))
	if user != nil {
		doc.Set("user", *user)
	}
	return must.NotFail(documentOpMsg(doc))
}

func x509Context(certificate *x509.Certificate) (context.Context, *conninfo.ConnInfo) {
	ci := conninfo.New()
	ci.SetUsesTLS(true)
	ci.SetPeerCertificate(certificate)
	return conninfo.Ctx(context.Background(), ci), ci
}

func requireCommandCode(t *testing.T, err error, code handlererrors.ErrorCode) {
	t.Helper()

	var commandErr *handlererrors.CommandError
	require.True(t, errors.As(err, &commandErr), "expected command error, got %v", err)
	require.Equal(t, code, commandErr.Code())
}

func TestX509SubjectUsesRawRDNOrder(t *testing.T) {
	certificate, expected := testX509Certificate(t)
	subject, err := x509Subject(certificate)
	require.NoError(t, err)
	require.Equal(t, expected, subject)
}

func TestAuthenticateX509EscapesNonASCII(t *testing.T) {
	h := authGateHandler(t, true)
	raw := must.NotFail(asn1.Marshal(pkix.RDNSequence{
		{{Type: commonNameOID, Value: "Zo\u00eb M\u00fcller"}},
		{{Type: organizationOID, Value: "Example"}},
	}))
	certificate := &x509.Certificate{RawSubject: raw}
	subject := "O=Example,CN=Zo\\C3\\AB M\\C3\\BCller"
	insertX509User(t, h, subject)
	ctx, ci := x509Context(certificate)

	_, err := h.commands["authenticate"].Handler(ctx, authenticateMsg(t, nil))
	require.NoError(t, err)
	require.True(t, ci.Authenticated())
	username, _, _, db := ci.Auth()
	require.Equal(t, subject, username)
	require.Equal(t, "$external", db)
}

func TestAuthenticateX509(t *testing.T) {
	h := authGateHandler(t, true)
	certificate, subject := testX509Certificate(t)
	insertX509User(t, h, subject)

	for _, claimedUser := range []*string{nil, pointerTo(""), &subject} {
		ctx, ci := x509Context(certificate)
		_, err := h.commands["authenticate"].Handler(ctx, authenticateMsg(t, claimedUser))
		require.NoError(t, err)
		require.True(t, ci.Authenticated())
		require.True(t, ci.BypassBackendAuth())
		username, _, _, db := ci.Auth()
		require.Equal(t, subject, username)
		require.Equal(t, "$external", db)

		_, err = h.commands["listDatabases"].Handler(ctx, gateCmd(t, "listDatabases"))
		require.False(t, isForcedLoginError(err), "X.509 identity must pass the auth gate: %v", err)
	}
}

func TestAuthenticateX509RejectsClaimedIdentity(t *testing.T) {
	h := authGateHandler(t, true)
	certificate, subject := testX509Certificate(t)
	insertX509User(t, h, subject)
	other := "CN=other"
	ctx, ci := x509Context(certificate)

	_, err := h.commands["authenticate"].Handler(ctx, authenticateMsg(t, &other))
	requireCommandCode(t, err, handlererrors.ErrAuthenticationFailed)
	require.False(t, ci.Authenticated())
}

func TestAuthenticateX509RequiresTLS(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	_, err := h.commands["authenticate"].Handler(ctx, authenticateMsg(t, nil))
	requireCommandCode(t, err, handlererrors.ErrAuthenticationFailed)
	require.Contains(t, err.Error(), "requires a TLS connection")
}

func TestAuthenticateX509RequiresCertificate(t *testing.T) {
	h := authGateHandler(t, true)
	ci := conninfo.New()
	ci.SetUsesTLS(true)
	ctx := conninfo.Ctx(context.Background(), ci)

	_, err := h.commands["authenticate"].Handler(ctx, authenticateMsg(t, nil))
	requireCommandCode(t, err, handlererrors.ErrAuthenticationFailed)
	require.Contains(t, err.Error(), "requires a verified client certificate")
}

func TestAuthenticateX509RequiresStoredUser(t *testing.T) {
	h := authGateHandler(t, true)
	certificate, subject := testX509Certificate(t)
	ctx, _ := x509Context(certificate)

	_, err := h.commands["authenticate"].Handler(ctx, authenticateMsg(t, nil))
	requireCommandCode(t, err, handlererrors.ErrUserNotFound)
	require.Contains(t, err.Error(), "Could not find user \""+subject+"\" for db \"$external\"")
}

func pointerTo(value string) *string {
	return &value
}
