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
	"net/netip"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) checkAuthRestrictions(ctx context.Context, db, user string) error {
	doc, err := h.loadUserDoc(ctx, db, user)
	if err != nil {
		return err
	}

	if doc == nil {
		return nil
	}

	raw, err := doc.Get("authenticationRestrictions")
	if err != nil {
		return nil
	}

	restrictions, ok := raw.(*types.Array)
	if !ok || restrictions.Len() == 0 {
		return nil
	}

	ci := conninfo.Get(ctx)
	clientAddr := ci.Peer.Addr()
	serverAddr := ci.Local.Addr()

	for i := 0; i < restrictions.Len(); i++ {
		rd, ok := must.NotFail(restrictions.Get(i)).(*types.Document)
		if ok && restrictionSatisfied(rd, clientAddr, serverAddr) {
			return nil
		}
	}

	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrAuthenticationFailed,
		"Authentication failed.",
		"saslContinue",
	)
}

func restrictionSatisfied(doc *types.Document, clientAddr, serverAddr netip.Addr) bool {
	if v, err := doc.Get("clientSource"); err == nil {
		if list, ok := v.(*types.Array); ok && !addrMatches(clientAddr, list) {
			return false
		}
	}

	if v, err := doc.Get("serverAddress"); err == nil {
		if list, ok := v.(*types.Array); ok && !addrMatches(serverAddr, list) {
			return false
		}
	}

	return true
}

func addrMatches(addr netip.Addr, list *types.Array) bool {
	if !addr.IsValid() {
		return false
	}

	for i := 0; i < list.Len(); i++ {
		entry, ok := must.NotFail(list.Get(i)).(string)
		if !ok {
			continue
		}

		if prefix, err := netip.ParsePrefix(entry); err == nil {
			if prefix.Contains(addr) {
				return true
			}
			continue
		}

		if a, err := netip.ParseAddr(entry); err == nil && a == addr {
			return true
		}
	}

	return false
}
