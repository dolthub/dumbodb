// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

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

// BSON storage format version. Currently a single byte 0x01 prepended
// to the document bytes. Reserves room for forward-compatible format
// changes after the bson-a-vs-bson-b bake-off picks a winner.
const bsonFormatVersion byte = 0x01

// docToBSON converts a types.Document to raw BSON bytes with object
// fields lex-sorted at every level. Replaces docToExtJSON in the
// bson-a write path; the version byte is prepended so the stored
// payload is [0x01][raw-BSON].
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

// bsonToDoc converts stored bson-a bytes back to a types.Document.
// Replaces decodeDocFromJSON in the read path.
func bsonToDoc(stored []byte) (*types.Document, error) {
	raw, err := stripVersion(stored)
	if err != nil {
		return nil, err
	}
	return decodeDocument(raw)
}

// prependVersion writes the format version byte in front of raw and
// returns the combined buffer. The caller must not retain raw after
// this call returns since the underlying bytes are copied.
func prependVersion(raw []byte) []byte {
	out := make([]byte, 0, len(raw)+1)
	out = append(out, bsonFormatVersion)
	out = append(out, raw...)
	return out
}

// stripVersion returns the raw BSON bytes after the version header.
// Rejects stored values without a recognised format byte; future
// format changes are flagged here so older readers fail loudly
// rather than silently misinterpret the payload.
func stripVersion(stored []byte) ([]byte, error) {
	if len(stored) < 1 {
		return nil, fmt.Errorf("bson stored value is empty")
	}
	if stored[0] != bsonFormatVersion {
		return nil, fmt.Errorf("unknown bson storage format version 0x%02x", stored[0])
	}
	return stored[1:], nil
}

// sortDocumentKeys returns a deep copy of doc with object keys
// lex-sorted at every level. Array element order is preserved.
// This is the canonical form the bson-a storage uses; sorting on
// write makes diff and merge well-defined.
func sortDocumentKeys(doc *types.Document) *types.Document {
	keys := append([]string(nil), doc.Keys()...)
	sort.Strings(keys)
	out := types.MakeDocument(len(keys))
	for _, k := range keys {
		v, _ := doc.Get(k)
		out.Set(k, sortAnyKeys(v))
	}
	return out
}

// sortAnyKeys recurses into nested documents and arrays, returning
// a value with sorted keys at every level. Scalars are returned
// unchanged.
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

// writeBSONDocToValue writes a types.Document to a value tuple as
// bson-a-format bytes. Replaces writeDocToValue in the call sites
// that have moved to the new format.
func writeBSONDocToValue(ctx context.Context, ns tree.NodeStore, doc *types.Document) (val.Tuple, error) {
	stored, err := docToBSON(doc)
	if err != nil {
		return nil, err
	}
	return buildValue(ctx, ns, stored)
}

// readBSONDocFromValue decodes a value tuple's stored bson-a bytes
// into a types.Document. Replaces readDocFromValue.
func readBSONDocFromValue(ctx context.Context, ns tree.NodeStore, v val.Tuple) (*types.Document, error) {
	stored, err := getBSONStoredBytes(ctx, ns, v)
	if err != nil {
		return nil, err
	}
	return bsonToDoc(stored)
}

// encodeBSONValue converts a Go value (one of the types
// FieldMutation.Value can carry) into a (type byte, value bytes)
// pair suitable for handing to bsonindexed.IndexedBsonDocument's
// SetField / PushArray APIs. The value bytes are stripped of the
// BSON type byte and field-name CString so the caller can splice
// them in alongside an arbitrary field name.
//
// Implementation: build a single-field wirebson document, encode it,
// and extract the value-only span via byte slicing. The encoded
// document layout is
//
//	[4-byte length][type byte][name][0x00][value][0x00]
//
// so the value bytes live at [5+len(name)+1, len(raw)-1).
//
// MinKey / MaxKey values are handled separately via bson.FromDocumentRaw
// because wirebson does not encode them.
func encodeBSONValue(value any) (byte, []byte, error) {
	const probeName = "v"

	if _, ok := value.(types.MinKeyType); ok {
		return 0xFF, nil, nil
	}
	if _, ok := value.(types.MaxKeyType); ok {
		return 0x7F, nil, nil
	}

	// MinKey / MaxKey nested anywhere inside the value forces the raw
	// encoder path. AnyContainsMinMaxKey scans nested arrays / docs.
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

// extractValueBytes pulls the value portion out of a single-field
// BSON document. The caller guarantees the document was constructed
// with one field whose name is name.
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

// getBSONStoredBytes returns the raw bson-a stored bytes for a value
// tuple (the version header is included so callers can interrogate
// it directly). Hides the AdaptiveValue inline-vs-OOB dispatch.
func getBSONStoredBytes(ctx context.Context, ns tree.NodeStore, v val.Tuple) ([]byte, error) {
	result, ok, err := valDesc.GetBytesAdaptiveValue(ctx, 0, ns, v)
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
