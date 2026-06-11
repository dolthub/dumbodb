// Copyright 2021 FerretDB Inc.
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
	"log/slog"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

var getParameterMetaKeys = map[string]struct{}{
	"getParameter":  {},
	"$db":           {},
	"comment":       {},
	"lsid":          {},
	"$clusterTime":  {},
	"$readPreference": {},
	"maxTimeMS":     {},
}

// MsgGetParameter implements `getParameter` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgGetParameter(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if db, _ := document.Get("$db"); db != "admin" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrUnauthorized,
			"getParameter may only be run against the admin database.",
			"getParameter",
		)
	}

	getParameter := must.NotFail(document.Get("getParameter"))

	showDetails, allParameters, err := extractGetParameter(getParameter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	common.Ignored(document, h.L, "comment")

	parameters := buildParameterDoc(h.paramStore)

	resDoc, err := selectParameters(document, parameters, showDetails, allParameters)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if unknown := unknownGetParameterKeys(document, parameters); len(unknown) > 0 {
		h.L.InfoContext(connCtx, "getParameter: unknown keys requested",
			slog.Any("keys", unknown),
		)
	}

	if resDoc.Len() < 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrorCode(72),
			"no option found to get",
			document.Command(),
		)
	}

	resDoc.Set("ok", float64(1))

	return documentOpMsg(
		resDoc,
	)
}

// selectParameters makes a selection of requested parameters.
func selectParameters(document, parameters *types.Document, showDetails, allParameters bool) (resDoc *types.Document, err error) {
	resDoc = must.NotFail(types.NewDocument())

	iter := parameters.Iterator()
	defer iter.Close()

	for {
		k, v, err := iter.Next()
		if err != nil {
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			return nil, lazyerrors.Error(err)
		}

		if !allParameters && !document.Has(k) {
			continue
		}

		if !showDetails {
			v = must.NotFail(v.(*types.Document).Get("value"))
		}

		resDoc.Set(k, v)
	}

	return resDoc, nil
}

// extractGetParameter retrieves showDetails & allParameters options set on the getParameter value.
func extractGetParameter(getParameter any) (showDetails, allParameters bool, err error) {
	if getParameter == "*" {
		allParameters = true
		return
	}

	if param, ok := getParameter.(*types.Document); ok {
		if v, _ := param.Get("showDetails"); v != nil {
			showDetails, err = handlerparams.GetBoolOptionalParam("getParameter.showDetails", v)
			if err != nil {
				return false, false, lazyerrors.Error(err)
			}
		}

		if v, _ := param.Get("allParameters"); v != nil {
			allParameters, err = handlerparams.GetBoolOptionalParam("getParameter.allParameters", v)
			if err != nil {
				return false, false, lazyerrors.Error(err)
			}
		}
	}

	return showDetails, allParameters, nil
}

func unknownGetParameterKeys(request, knownParams *types.Document) []string {
	var unknown []string
	for _, k := range request.Keys() {
		if _, meta := getParameterMetaKeys[k]; meta {
			continue
		}
		if knownParams.Has(k) {
			continue
		}
		unknown = append(unknown, k)
	}
	return unknown
}
