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

package dolt

import (
	"context"
	"fmt"
	"sort"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
)

// Stored format: [bsonFormatVersion][raw BSON]. Reserves room for a
// breaking format change after a wire-format incompatibility.
const bsonFormatVersion byte = 0x01

// docToBSON encodes doc with keys lex-sorted at every level (canonical
// form for diff and merge), prepended with bsonFormatVersion.
func docToBSON(doc *types.Document) ([]byte, error) {
	sorted := sortDocumentKeys(doc)
	if docHasMinMaxKey(sorted) {
		raw, err := bson.FromDocumentRaw(sorted)
		if err != nil {
			return nil, fmt.Errorf("encoding document with MinKey/MaxKey to BSON: %w", err)
		}
		return prependVersion(raw), nil
	}
	wdoc, err := bson.FromDocument(sorted)
	if err != nil {
		return nil, fmt.Errorf("encoding document to wirebson: %w", err)
	}
	raw, err := wdoc.Encode()
	if err != nil {
		return nil, fmt.Errorf("encoding wirebson document: %w", err)
	}
	return prependVersion(raw), nil
}

func bsonToDoc(stored []byte) (*types.Document, error) {
	raw, err := stripVersion(stored)
	if err != nil {
		return nil, err
	}
	return decodeDocument(raw)
}

func prependVersion(raw []byte) []byte {
	out := make([]byte, 0, len(raw)+1)
	out = append(out, bsonFormatVersion)
	out = append(out, raw...)
	return out
}

// stripVersion rejects unrecognised version bytes so future format
// changes fail loudly rather than silently misinterpret the payload.
func stripVersion(stored []byte) ([]byte, error) {
	if len(stored) < 1 {
		return nil, fmt.Errorf("bson stored value is empty")
	}
	if stored[0] != bsonFormatVersion {
		return nil, fmt.Errorf("unknown bson storage format version 0x%02x", stored[0])
	}
	return stored[1:], nil
}

// sortDocumentKeys deep-copies doc with object keys lex-sorted at every
// level. Top-level _id is preserved verbatim: MongoDB treats {a:1,b:2}
// and {b:2,a:1} as distinct _ids, and hashID hashes the original byte
// order; sorting would break post-lookup filter equality.
func sortDocumentKeys(doc *types.Document) *types.Document {
	keys := append([]string(nil), doc.Keys()...)
	sort.Strings(keys)
	out := types.MakeDocument(len(keys))
	for _, k := range keys {
		v, _ := doc.Get(k)
		if k == "_id" {
			out.Set(k, v)
			continue
		}
		out.Set(k, sortAnyKeys(v))
	}
	return out
}

func sortAnyKeys(v any) any {
	switch t := v.(type) {
	case *types.Document:
		return sortDocumentKeys(t)
	case *types.Array:
		out := types.MakeArray(t.Len())
		for i := 0; i < t.Len(); i++ {
			el, _ := t.Get(i)
			out.Append(sortAnyKeys(el))
		}
		return out
	default:
		return v
	}
}

func writeBSONDocToValue(ctx context.Context, ns tree.NodeStore, doc *types.Document) (val.Tuple, error) {
	stored, err := docToBSON(doc)
	if err != nil {
		return nil, err
	}
	return buildValue(ctx, ns, stored)
}

func readBSONDocFromValue(ctx context.Context, ns tree.NodeStore, v val.Tuple) (*types.Document, error) {
	stored, err := getBSONStoredBytes(ctx, ns, v)
	if err != nil {
		return nil, err
	}
	return bsonToDoc(stored)
}

// encodeBSONValue extracts (typeByte, valueBytes) for value by building
// a single-field wirebson document and slicing out the value span. The
// returned valueBytes have the BSON type byte and field-name CString
// stripped so callers can splice them under any field name.
func encodeBSONValue(value any) (byte, []byte, error) {
	const probeName = "v"

	if _, ok := value.(types.MinKeyType); ok {
		return 0xFF, nil, nil
	}
	if _, ok := value.(types.MaxKeyType); ok {
		return 0x7F, nil, nil
	}

	// wirebson does not encode MinKey/MaxKey; route those (even when
	// nested) through bson.FromDocumentRaw.
	if bson.AnyContainsMinMaxKey(value) {
		tmp, err := types.NewDocument(probeName, value)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding value with MinKey/MaxKey: %w", err)
		}
		raw, err := bson.FromDocumentRaw(tmp)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding value with MinKey/MaxKey: %w", err)
		}
		return extractValueBytes(raw, probeName)
	}

	tmp := wirebson.MakeDocument(1)
	if err := tmp.Add(probeName, value); err != nil {
		return 0, nil, fmt.Errorf("adding value to wirebson doc: %w", err)
	}
	raw, err := tmp.Encode()
	if err != nil {
		return 0, nil, fmt.Errorf("encoding wirebson doc: %w", err)
	}
	return extractValueBytes([]byte(raw), probeName)
}

func extractValueBytes(raw []byte, name string) (byte, []byte, error) {
	if len(raw) < 4+1+len(name)+1+1 {
		return 0, nil, fmt.Errorf("bson value buffer too short: %d", len(raw))
	}
	typeByte := raw[4]
	valueStart := 4 + 1 + len(name) + 1
	valueEnd := len(raw) - 1
	if valueEnd < valueStart {
		return 0, nil, fmt.Errorf("bson value layout invalid")
	}
	out := make([]byte, valueEnd-valueStart)
	copy(out, raw[valueStart:valueEnd])
	return typeByte, out, nil
}

// getBSONStoredBytes hides the inline-vs-OOB dispatch of the value tuple.
func getBSONStoredBytes(ctx context.Context, ns tree.NodeStore, v val.Tuple) ([]byte, error) {
	result, ok, err := valDescFor(ns).GetBytesAdaptiveValue(ctx, 0, ns, v)
	if err != nil {
		return nil, fmt.Errorf("reading bytes value from tuple: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("value tuple missing bytes field")
	}
	switch existing := result.(type) {
	case []byte:
		return existing, nil
	case *val.ByteArray:
		return existing.GetBytes(ctx)
	default:
		return nil, fmt.Errorf("unexpected BytesAdaptiveValue type %T", result)
	}
}
