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
	"unicode/utf8"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

const (
	// errorMessagesBudget bounds the error text one reply repeats in full:
	// once the messages before an error reach it, that error's message is
	// shortened, so a batch of large refusals cannot bloat the reply.
	errorMessagesBudget = 3 << 20

	// shortenedErrorMessage is how much of a message past the budget is kept.
	shortenedErrorMessage = 1 << 10
)

// capErrorMessages shortens, in place, the "errmsg" of each document in
// errs once the messages before it reach errorMessagesBudget.
func capErrorMessages(errs *types.Array) {
	total := 0
	for i := range errs.Len() {
		doc, ok := must.NotFail(errs.Get(i)).(*types.Document)
		if !ok {
			continue
		}
		v, err := doc.Get("errmsg")
		if err != nil {
			continue
		}
		msg, ok := v.(string)
		if !ok {
			continue
		}
		if total >= errorMessagesBudget && len(msg) > shortenedErrorMessage {
			cut := shortenedErrorMessage
			for cut > 0 && !utf8.RuneStart(msg[cut]) {
				cut--
			}
			doc.Set("errmsg", msg[:cut]+"...")
		}
		total += len(msg)
	}
}
