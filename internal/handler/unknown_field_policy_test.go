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
	"errors"
	"testing"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestUnknownFieldPolicyCoverage is the guardrail: every registered command must
// be classified in unknownFieldPolicies, and every classification must name a
// registered command. Adding a new version-control command or enabling a new
// MongoDB command fails here until it is classified -- forcing the strict-vs-
// legacy decision to be made deliberately (default: strictRejects).
func TestUnknownFieldPolicyCoverage(t *testing.T) {
	h := handlerForTest(t)
	cmds := h.Commands()

	for name := range cmds {
		if _, ok := unknownFieldPolicies[name]; !ok {
			t.Errorf("command %q is registered but not classified in unknownFieldPolicies.\n"+
				"Classify it in unknown_field_policy.go: DEFAULT to strictRejects and wire "+
				"common.RejectUnknownFields into its handler; use legacyAccepts only if the parity "+
				"suite confirms MongoDB accepts unknown fields on it; use strictPending to defer "+
				"(with a beads task).", name)
		}
	}

	for name := range unknownFieldPolicies {
		if _, ok := cmds[name]; !ok {
			t.Errorf("unknownFieldPolicies classifies %q, but it is not a registered command "+
				"(stale entry -- remove it from unknown_field_policy.go).", name)
		}
	}
}

// TestStrictCommandsRejectUnknownField gives the policy teeth: every command
// classified strictRejects must actually reject an unknown top-level field with
// ErrIDLUnknownField. A strictRejects command whose handler forgot to wire
// common.RejectUnknownFields fails here.
func TestStrictCommandsRejectUnknownField(t *testing.T) {
	h := handlerForTest(t)
	cmds := h.Commands()
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	for name, policy := range unknownFieldPolicies {
		if policy != strictRejects {
			continue
		}
		cmd, ok := cmds[name]
		if !ok {
			continue // reported by TestUnknownFieldPolicyCoverage
		}

		t.Run(name, func(t *testing.T) {
			// The unknown-field check runs before any parameter/target validation,
			// so a bogus field alone triggers it. A string command value keeps the
			// CRUD (ExtractParams) decoders from type-erroring on the value first.
			doc := must.NotFail(types.NewDocument(
				name, "c",
				"$db", "ufpolicytestdb",
				"nonExistentField42", int32(1),
			))
			_, err := cmd.Handler(ctx, must.NotFail(documentOpMsg(doc)))
			if err == nil {
				t.Fatalf("command %q (strictRejects) accepted an unknown field; wire common.RejectUnknownFields into its handler or reclassify", name)
			}

			var ce *handlererrors.CommandError
			if !errors.As(err, &ce) || ce.Code() != handlererrors.ErrIDLUnknownField {
				t.Fatalf("command %q (strictRejects) must reject an unknown field with IDLUnknownField (40415); got %v", name, err)
			}
		})
	}
}
