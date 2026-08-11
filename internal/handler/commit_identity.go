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
	"fmt"
	"strings"

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
