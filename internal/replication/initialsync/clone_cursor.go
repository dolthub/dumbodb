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

package initialsync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
)

// CloneCursor identifies a resumable collection scan by source UUID.
type CloneCursor struct {
	Database    string
	SourceUUID  types.Binary
	ResumeAfter *types.Document
}

// CloneDocuments scans a source collection in natural order and returns its final resume token.
func CloneDocuments(ctx context.Context, client requestClient, cursor CloneCursor, consume func(*types.Document) error) (*types.Document, error) {
	if client == nil || cursor.Database == "" || cursor.SourceUUID.Subtype != types.BinaryUUID || len(cursor.SourceUUID.B) != 16 || consume == nil {
		return nil, errors.New("collection clone requires client, database, UUID, and consumer")
	}
	response, err := client.Request(ctx, cloneFindRequest(cursor))
	if err != nil {
		return cursor.ResumeAfter, err
	}
	lastToken := cursor.ResumeAfter
	for {
		document, err := responseDocument(response)
		if err != nil {
			return lastToken, err
		}
		batch, token, cursorID, collection, err := cloneBatch(document)
		if err != nil {
			return lastToken, err
		}
		for index := 0; index < batch.Len(); index++ {
			value, _ := batch.Get(index)
			document, ok := value.(*types.Document)
			if !ok {
				return lastToken, fmt.Errorf("clone cursor document has type %T, want document", value)
			}
			if err := consume(document); err != nil {
				return lastToken, err
			}
		}
		if token != nil {
			lastToken = token
		}
		if cursorID == 0 {
			return lastToken, nil
		}
		response, err = client.Request(ctx, cloneGetMoreRequest(cursorID, collection, cursor.Database))
		if err != nil {
			return lastToken, err
		}
	}
}

func cloneFindRequest(cursor CloneCursor) *wire.OpMsg {
	hint := wirebson.MakeDocument(1)
	_ = hint.Add("$natural", int32(1))
	readConcern := wirebson.MakeDocument(1)
	_ = readConcern.Add("level", "local")
	readPreference := wirebson.MakeDocument(1)
	_ = readPreference.Add("mode", "secondaryPreferred")
	arguments := []any{
		"find", wirebson.Binary{Subtype: wirebson.BinarySubtype(cursor.SourceUUID.Subtype), B: cursor.SourceUUID.B},
		"hint", hint, "noCursorTimeout", true, "$_requestResumeToken", true,
		"readConcern", readConcern, "$readPreference", readPreference,
	}
	if cursor.ResumeAfter != nil {
		arguments = append(arguments, "resumeAfter", mustWireDocument(cursor.ResumeAfter))
	}
	arguments = append(arguments, "$db", cursor.Database)
	return wire.MustOpMsg(arguments...)
}

func cloneGetMoreRequest(cursorID int64, collection, database string) *wire.OpMsg {
	readPreference := wirebson.MakeDocument(1)
	_ = readPreference.Add("mode", "secondaryPreferred")
	return wire.MustOpMsg(
		"getMore", cursorID, "collection", collection,
		"$readPreference", readPreference, "$db", database,
	)
}

func cloneBatch(document *types.Document) (*types.Array, *types.Document, int64, string, error) {
	cursorValue, _ := document.Get("cursor")
	cursor, ok := cursorValue.(*types.Document)
	if !ok {
		return nil, nil, 0, "", fmt.Errorf("clone response cursor has type %T, want document", cursorValue)
	}
	batchValue, batchErr := cursor.Get("firstBatch")
	if batchErr != nil {
		batchValue, _ = cursor.Get("nextBatch")
	}
	batch, ok := batchValue.(*types.Array)
	if !ok {
		return nil, nil, 0, "", fmt.Errorf("clone response batch has type %T, want array", batchValue)
	}
	idValue, _ := cursor.Get("id")
	cursorID, err := integer(idValue)
	if err != nil {
		return nil, nil, 0, "", fmt.Errorf("clone cursor id: %w", err)
	}
	namespaceValue, _ := cursor.Get("ns")
	namespace, ok := namespaceValue.(string)
	if !ok {
		return nil, nil, 0, "", fmt.Errorf("clone cursor namespace has type %T, want string", namespaceValue)
	}
	dot := strings.IndexByte(namespace, '.')
	if dot <= 0 || dot == len(namespace)-1 {
		return nil, nil, 0, "", fmt.Errorf("clone cursor has invalid namespace %q", namespace)
	}
	var token *types.Document
	if tokenValue, tokenErr := cursor.Get("postBatchResumeToken"); tokenErr == nil {
		token, ok = tokenValue.(*types.Document)
		if !ok {
			return nil, nil, 0, "", fmt.Errorf("clone resume token has type %T, want document", tokenValue)
		}
	}
	return batch, token, cursorID, namespace[dot+1:], nil
}

func responseDocument(response *wire.OpMsg) (*types.Document, error) {
	raw, err := response.RawDocument()
	if err != nil {
		return nil, err
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		return nil, err
	}
	okValue, _ := document.Get("ok")
	if ok, valid := okValue.(float64); !valid || ok != 1 {
		message, _ := document.Get("errmsg")
		return nil, fmt.Errorf("MongoDB clone command failed: %v", message)
	}
	return document, nil
}

func mustWireDocument(document *types.Document) *wirebson.Document {
	result, err := bson.FromDocument(document)
	if err != nil {
		panic(err)
	}
	return result
}
