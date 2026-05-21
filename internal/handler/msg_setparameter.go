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
	"fmt"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// setParameterEnvelopeFields are wire/session fields that may appear in a
// setParameter command document but are not themselves parameters to set.
var setParameterEnvelopeFields = map[string]bool{
	"setParameter":         true,
	"$db":                  true,
	"lsid":                 true,
	"$clusterTime":         true,
	"$readPreference":      true,
	"txnNumber":            true,
	"autocommit":           true,
	"startTransaction":     true,
	"apiVersion":           true,
	"apiStrict":            true,
	"apiDeprecationErrors": true,
	"comment":              true,
}

// MsgSetParameter implements the `setParameter` admin command.
//
// Accepts one or more parameters per call:
//   db.adminCommand({setParameter: 1, paramA: valueA, paramB: valueB})
// Returns the previous value of the last-updated parameter under "was",
// matching MongoDB's wire shape for the single-parameter case. Unknown
// parameters return code 72 (InvalidOptions); known-but-not-settable-at-
// runtime parameters return code 13631.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgSetParameter(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	dbName, _ := document.Get("$db")
	if db, ok := dbName.(string); !ok || db != "admin" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrUnauthorized,
			"setParameter may only be run against the admin database.",
			"setParameter",
		)
	}

	res := must.NotFail(types.NewDocument())
	var (
		anySet   bool
		lastPrev any
	)

	iter := document.Iterator()
	defer iter.Close()
	for {
		k, v, err := iter.Next()
		if err != nil {
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}
			return nil, lazyerrors.Error(err)
		}
		if setParameterEnvelopeFields[k] {
			continue
		}
		prev, code := h.paramStore.Set(k, v)
		switch code {
		case ParamSetUnknown:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidOptions,
				fmt.Sprintf("Unknown setParameter option called: %s", k),
				k,
			)
		case ParamSetNotRuntime:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrorCode(13631),
				fmt.Sprintf("Server parameter %q is not settable at runtime", k),
				k,
			)
		}
		lastPrev = prev
		anySet = true
	}

	if anySet {
		res.Set("was", lastPrev)
	}
	res.Set("ok", float64(1))
	return documentOpMsg(res)
}
