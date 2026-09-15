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

// Package oplog fetches and buffers MongoDB replication oplog entries.
package oplog

import (
	"fmt"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
)

type Entry struct {
	OpTime    control.OpTime
	Operation string
	Namespace string
	RawBSON   []byte
}

func ParseEntry(document *types.Document) (Entry, error) {
	timestampValue, _ := document.Get("ts")
	timestamp, ok := timestampValue.(types.Timestamp)
	if !ok {
		return Entry{}, fmt.Errorf("oplog entry ts has type %T, want timestamp", timestampValue)
	}
	termValue, _ := document.Get("t")
	term, err := entryInteger(termValue)
	if err != nil {
		return Entry{}, fmt.Errorf("oplog entry t: %w", err)
	}
	operationValue, _ := document.Get("op")
	operation, ok := operationValue.(string)
	if !ok || operation == "" {
		return Entry{}, fmt.Errorf("oplog entry op has type %T, want non-empty string", operationValue)
	}
	namespaceValue, _ := document.Get("ns")
	namespace, ok := namespaceValue.(string)
	if !ok {
		return Entry{}, fmt.Errorf("oplog entry ns has type %T, want string", namespaceValue)
	}
	raw, err := bson.FromDocumentRaw(document)
	if err != nil {
		return Entry{}, fmt.Errorf("encoding oplog entry: %w", err)
	}
	return Entry{
		OpTime: control.OpTime{
			Seconds:   uint32(uint64(timestamp) >> 32),
			Increment: uint32(timestamp),
			Term:      term,
		},
		Operation: operation,
		Namespace: namespace,
		RawBSON:   raw,
	}, nil
}

func entryInteger(value any) (int64, error) {
	switch value := value.(type) {
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	default:
		return 0, fmt.Errorf("has type %T, want integer", value)
	}
}
