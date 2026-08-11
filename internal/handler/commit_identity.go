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
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// validateCommitName checks a commit-identity name. The rules are only enough to
// guarantee the value round-trips into Dolt's "Name <email>" form and cannot
// smuggle a second identity, not full display-name validation.
func validateCommitName(name string) error {
	if name == "" {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "commitIdentity.name must not be empty")
	}

	if strings.ContainsAny(name, "<>") {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "commitIdentity.name must not contain '<' or '>'")
	}

	return nil
}

// validateCommitEmail checks a commit-identity email. Not full RFC 5322: just
// enough that the value cannot corrupt the "Name <email>" serialization or hold a
// second address.
func validateCommitEmail(email string) error {
	if email == "" {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "commitIdentity.email must not be empty")
	}

	if strings.ContainsAny(email, "<> \t\r\n") {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "commitIdentity.email must not contain whitespace, '<', or '>'")
	}

	at := strings.IndexByte(email, '@')
	if at < 0 || at != strings.LastIndexByte(email, '@') {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "commitIdentity.email must contain exactly one '@'")
	}

	if at == 0 || at == len(email)-1 {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "commitIdentity.email must have a non-empty local part and domain")
	}

	return nil
}

// validateCommitIdentity validates a full name/email identity.
func validateCommitIdentity(name, email string) error {
	if err := validateCommitName(name); err != nil {
		return err
	}

	return validateCommitEmail(email)
}

// parseCommitIdentity extracts and validates the optional commitIdentity document
// from a create/update command. It returns nil when the field is absent, null, or
// an empty document. Present name/email subfields are each validated; a partial
// identity (name-only or email-only) is allowed and completed at resolution time.
func parseCommitIdentity(document *types.Document) (*types.Document, error) {
	v, err := document.Get("commitIdentity")
	if err != nil || v == nil || v == types.Null {
		return nil, nil
	}

	doc, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf("BSON field 'commitIdentity' is the wrong type '%s', expected type 'object'",
				handlerparams.AliasFromType(v),
			),
		)
	}

	for _, k := range doc.Keys() {
		if k != "name" && k != "email" {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrBadValue,
				fmt.Sprintf("BSON field 'commitIdentity.%s' is an unknown field", k),
			)
		}
	}

	out := types.MakeDocument(0)

	if doc.Has("name") {
		nv, _ := doc.Get("name")

		name, ok := nv.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("BSON field 'commitIdentity.name' is the wrong type '%s', expected type 'string'",
					handlerparams.AliasFromType(nv),
				),
			)
		}

		if err = validateCommitName(name); err != nil {
			return nil, err
		}

		out.Set("name", name)
	}

	if doc.Has("email") {
		ev, _ := doc.Get("email")

		email, ok := ev.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("BSON field 'commitIdentity.email' is the wrong type '%s', expected type 'string'",
					handlerparams.AliasFromType(ev),
				),
			)
		}

		if err = validateCommitEmail(email); err != nil {
			return nil, err
		}

		out.Set("email", email)
	}

	if out.Len() == 0 {
		return nil, nil
	}

	return out, nil
}

// resolveCommitIdentity returns the commit identity (name, email) for the
// authenticated connection, reading the user's stored commitIdentity and filling
// any missing piece from the auth identity (name <- username, email <-
// username@authDb). ok is false when the connection is unauthenticated, in which
// case callers fall back to the legacy author/default behavior. The result is
// cached on the connection and invalidated by the auth generation counter.
func (h *Handler) resolveCommitIdentity(ctx context.Context) (name, email string, ok bool, err error) {
	ci := conninfo.Get(ctx)

	user, _, _, userDB := ci.Auth()
	if user == "" {
		return "", "", false, nil
	}

	gen := h.authGen.Load()
	if n, e, cgen, cok := ci.CommitIdentityCache(); cok && cgen == gen {
		return n, e, true, nil
	}

	storedName, storedEmail, err := h.loadUserCommitIdentity(ctx, userDB, user)
	if err != nil {
		return "", "", false, err
	}

	name, email = commitIdentityWithFallback(storedName, storedEmail, user, userDB)

	ci.SetCommitIdentityCache(gen, name, email)

	return name, email, true, nil
}

// commitIdentityWithFallback fills any missing identity piece from the auth
// identity: an empty name becomes user, an empty email becomes user@db.
func commitIdentityWithFallback(name, email, user, db string) (string, string) {
	if name == "" {
		name = user
	}
	if email == "" {
		email = user + "@" + db
	}
	return name, email
}

// loadUserCommitIdentity reads the stored commitIdentity {name,email} for a user.
// Missing fields come back as empty strings; the caller applies the fallback.
func (h *Handler) loadUserCommitIdentity(ctx context.Context, db, user string) (name, email string, err error) {
	doc, err := h.loadUserDoc(ctx, db, user)
	if err != nil || doc == nil {
		return "", "", err
	}

	idVal, _ := doc.Get("commitIdentity")
	id, ok := idVal.(*types.Document)
	if !ok {
		return "", "", nil
	}

	if nv, _ := id.Get("name"); nv != nil {
		name, _ = nv.(string)
	}
	if ev, _ := id.Get("email"); ev != nil {
		email, _ = ev.(string)
	}

	return name, email, nil
}

// commitIdentityString returns the resolved identity as a "Name <email>" string
// for the authenticated connection, or ok=false when unauthenticated.
func (h *Handler) commitIdentityString(ctx context.Context) (string, bool, error) {
	name, email, ok, err := h.resolveCommitIdentity(ctx)
	if err != nil || !ok {
		return "", ok, err
	}

	return name + " <" + email + ">", true, nil
}
