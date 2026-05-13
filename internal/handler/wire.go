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
	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// init leaves wire.CheckNaNs at its default (false) so that incoming messages
// containing NaN float64 values (e.g. in query filters like {$mod: [NaN, 0]})
// are not rejected at the wire layer. NaN values in filter operators are
// validated and rejected with the correct error code (ErrBadValue / code 2)
// inside the individual filter handlers (see filterFieldMod, etc.).

// opMsgDocument gets a raw document from section 0 and converts to [*types.Document].
// Then it iterates raw documents from sections 1 if any, appends them
// to the response using the section identifier as the key.
func opMsgDocument(msg *wire.OpMsg) (*types.Document, error) {
	res, err := bson.ToDocument(msg.RawSection0())
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	for _, section := range msg.Sections() {
		if section.Kind == 0 {
			continue
		}

		docs := section.Documents()
		a := types.MakeArray(len(docs))

		for _, d := range docs {
			var doc *types.Document

			if doc, err = bson.ToDocument(d); err != nil {
				return nil, lazyerrors.Error(err)
			}

			a.Append(doc)
		}

		res.Set(section.Identifier, a)
	}

	return res, nil
}

func documentOpMsg(doc *types.Document) (*wire.OpMsg, error) {
	return wire.NewOpMsg(must.NotFail(bson.FromDocument(doc)))
}
