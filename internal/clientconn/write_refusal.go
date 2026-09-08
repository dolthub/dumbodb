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

package clientconn

import (
	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
)

// writeCountFields are the counters a write command reports. A refusal
// discards the command's whole overlay, so each of them becomes zero: the
// reply describes what reached the branch, and nothing did.
var writeCountFields = []string{
	"n", "nModified", "nInserted", "nMatched", "nUpserted", "nRemoved", "nDeleted",
}

// refusedWriteReply rewrites a write command's reply to report that nothing
// matched, and reports whether it could.
//
// This is MongoDB's answer for a compare-and-swap whose precondition no
// longer holds: {ok: 1, n: 0, nModified: 0} and no error, because the client
// branches on matchedCount. A refusal is not a failure, and while the
// boundary is still inside the command the reply has not been sent, so it can
// carry the right answer instead of an exception.
//
// Reports false if the reply cannot be rewritten, and the caller then keeps
// the error rather than inventing a success.
func refusedWriteReply(resMsg *wire.OpMsg) (*wire.OpMsg, bool) {
	if resMsg == nil {
		return nil, false
	}
	raw, err := resMsg.RawDocument()
	if err != nil {
		return nil, false
	}
	doc, err := raw.Decode()
	if err != nil {
		return nil, false
	}

	rewrote := false
	for _, field := range writeCountFields {
		if doc.Get(field) == nil {
			continue
		}
		if err := doc.Replace(field, int32(0)); err != nil {
			return nil, false
		}
		rewrote = true
	}

	// findAndModify reports the count inside lastErrorObject and returns the
	// document itself, so both have to say the write did not happen.
	if raw, ok := doc.Get("lastErrorObject").(wirebson.RawDocument); ok {
		leo, err := raw.Decode()
		if err != nil {
			return nil, false
		}
		if leo.Get("n") != nil {
			if err := leo.Replace("n", int32(0)); err != nil {
				return nil, false
			}
			rewrote = true
		}
		if leo.Get("updatedExisting") != nil {
			if err := leo.Replace("updatedExisting", false); err != nil {
				return nil, false
			}
		}
		if err := doc.Replace("lastErrorObject", leo); err != nil {
			return nil, false
		}
	}
	if doc.Get("value") != nil {
		if err := doc.Replace("value", wirebson.Null); err != nil {
			return nil, false
		}
	}

	// Nothing was upserted either, so an upserted array would contradict n:0.
	if doc.Get("upserted") != nil {
		doc.Remove("upserted")
	}

	if !rewrote {
		// No counter to rewrite means this is not a reply whose shape can
		// express "nothing matched".
		return nil, false
	}

	out, err := wire.NewOpMsg(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}
