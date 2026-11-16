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
	"go.mongodb.org/mongo-driver/v2/mongo"

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

// errorBudget applies the rule of capErrorMessages to messages one at a time,
// in reply order, so a long batch never holds more than the budget.
type errorBudget struct {
	total, large int
}

// shorten returns msg, or "" when mongod would empty it or the budget is spent.
func (b *errorBudget) shorten(msg string) string {
	isLarge := len(msg) > largeErrorMessage
	if b.total >= errorMessagesBudget || (isLarge && b.large >= keptLargeErrorMessages) {
		return ""
	}
	if isLarge {
		b.large++
	}
	b.total += len(msg)
	return msg
}

// appendError appends we to errs with its message shortened.
func (b *errorBudget) appendError(errs []*mongo.WriteError, we *mongo.WriteError) []*mongo.WriteError {
	we.Message = b.shorten(we.Message)
	return append(errs, we)
}

// capErrorMessages empties, in place, the "errmsg" of each document in errs
// that mongod would empty, and of each past errorMessagesBudget.
func capErrorMessages(errs *types.Array) {
	var b errorBudget
	for i := range errs.Len() {
		doc, ok := must.NotFail(errs.Get(i)).(*types.Document)
		if !ok {
			continue
		}
		v, err := doc.Get("errmsg")
		if err != nil {
			continue
		}
		if msg, ok := v.(string); ok {
			doc.Set("errmsg", b.shorten(msg))
		}
	}
}
