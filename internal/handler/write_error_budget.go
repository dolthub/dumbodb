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
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

const (
	// largeErrorMessage is the length past which mongod keeps a message in
	// full only for the first keptLargeErrorMessages of a reply and empties
	// the rest.
	largeErrorMessage      = 1 << 20
	keptLargeErrorMessages = 2

	// errorMessagesBudget bounds the error text of one reply: once the
	// messages before an error reach it, that error's message is emptied, so
	// the reply stays well under the 16 MiB a document may hold.
	errorMessagesBudget = 8 << 20
)

// capErrorMessages empties, in place, the "errmsg" of each document in errs
// that mongod would empty, and of each past errorMessagesBudget.
func capErrorMessages(errs *types.Array) {
	total, large := 0, 0
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
		isLarge := len(msg) > largeErrorMessage
		if total >= errorMessagesBudget || (isLarge && large >= keptLargeErrorMessages) {
			doc.Set("errmsg", "")
			continue
		}
		if isLarge {
			large++
		}
		total += len(msg)
	}
}
