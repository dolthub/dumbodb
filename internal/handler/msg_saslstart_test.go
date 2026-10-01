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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func scramStartWithUsername(t *testing.T, db, username string) *types.Document {
	t.Helper()

	return must.NotFail(types.NewDocument(
		"saslStart", int32(1),
		"mechanism", "SCRAM-SHA-256",
		"payload", types.Binary{B: []byte("n,,n=" + username + ",r=abcdefgh")},
		"$db", db,
	))
}

func TestSASLStartDecodesUsername(t *testing.T) {
	for _, test := range []struct {
		stored string
		wire   string
	}{
		{stored: "a,b=c", wire: "a=2Cb=3Dc"},
		{stored: "=2C", wire: "=3D2C"},
	} {
		t.Run(test.stored, func(t *testing.T) {
			h := authGateHandler(t, true)
			ctx := roleCtx()
			require.NoError(t, createUser(ctx, h, "mydb", test.stored, nil))

			_, err := h.MsgSASLStart(ctx, must.NotFail(documentOpMsg(scramStartWithUsername(t, "mydb", test.wire))))
			require.NoError(t, err)
			username, _, conversation, db := conninfo.Get(ctx).Auth()
			require.Equal(t, test.stored, username)
			require.NotNil(t, conversation)
			require.Equal(t, "mydb", db)
		})
	}
}

func TestSASLStartCredentiallessUserReturnsMechanismUnavailable(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()
	subject := "O=Example,OU=research,CN=analyst"
	_, err := h.MsgCreateUser(ctx, must.NotFail(documentOpMsg(createX509UserMsg(t, subject, types.MakeArray(0)))))
	require.NoError(t, err)

	_, err = h.MsgSASLStart(ctx, must.NotFail(documentOpMsg(scramStartWithUsername(
		t,
		"$external",
		"O=3DExample=2COU=3Dresearch=2CCN=3Danalyst",
	))))
	requireCommandCode(t, err, handlererrors.ErrMechanismUnavailable)
}
