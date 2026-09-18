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

package oplog

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/special"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// AffectedDatabases returns the databases whose working roots Apply will mutate.
func (a *Applier) AffectedDatabases(ctx context.Context, entry Entry) ([]string, error) {
	document, operation, err := decodeEntryOperation(entry)
	if err != nil {
		return nil, err
	}
	databases, err := a.affectedEntryDatabases(ctx, entry, document, operation)
	if err != nil {
		return nil, err
	}
	slices.Sort(databases)
	return slices.Compact(databases), nil
}

func decodeEntryOperation(entry Entry) (*types.Document, operation, error) {
	document, err := bson.ToDocument(wirebson.RawDocument(entry.RawBSON))
	if err != nil {
		return nil, operation{}, fmt.Errorf("decoding oplog entry at %v: %w", entry.OpTime, err)
	}
	parsed, err := parseOperation(document)
	if err != nil {
		return nil, operation{}, fmt.Errorf("parsing oplog entry at %v: %w", entry.OpTime, err)
	}
	if parsed.Kind != entry.Operation || parsed.Namespace != entry.Namespace {
		return nil, operation{}, errors.New("oplog entry fields disagree with parsed raw BSON")
	}
	return document, parsed, nil
}

func (a *Applier) affectedEntryDatabases(ctx context.Context, entry Entry, document *types.Document, outer operation) ([]string, error) {
	if outer.Kind != "c" {
		return a.affectedOperationDatabases(ctx, outer)
	}
	commandName, _, err := firstField(outer.Object)
	if err != nil {
		return nil, err
	}
	if commandName != "applyOps" && commandName != "commitTransaction" && commandName != "abortTransaction" {
		return a.affectedOperationDatabases(ctx, outer)
	}
	return a.affectedTransactionDatabases(ctx, entry, document, outer, commandName)
}

func (a *Applier) affectedOperationDatabases(ctx context.Context, operation operation) ([]string, error) {
	if special.IsIgnoredNamespace(operation.Namespace) {
		return nil, nil
	}
	if special.IsSpecialNamespace(operation.Namespace) {
		return []string{"admin"}, nil
	}
	switch operation.Kind {
	case "n":
		return nil, nil
	case "i", "u", "d":
		return a.databaseForSourceUUID(ctx, operation.SourceUUID, operation.Namespace)
	case "c":
		return a.affectedCommandDatabases(ctx, operation)
	default:
		return nil, fmt.Errorf("%w %q at %q", ErrUnsupportedOplogOperation, operation.Kind, operation.Namespace)
	}
}

func (a *Applier) affectedCommandDatabases(ctx context.Context, operation operation) ([]string, error) {
	database, collection, err := splitNamespace(operation.Namespace)
	if err != nil {
		return nil, err
	}
	if collection != "$cmd" {
		return nil, fmt.Errorf("command oplog namespace %q does not end in .$cmd", operation.Namespace)
	}
	commandName, commandValue, err := firstField(operation.Object)
	if err != nil {
		return nil, err
	}
	if target, ok := commandValue.(string); ok && special.IsIgnoredNamespace(database+"."+target) {
		return nil, nil
	}
	if database == "admin" || database == "config" {
		return nil, fmt.Errorf("%w command at reserved database %q", special.ErrUnsupportedSpecialNamespace, database)
	}
	switch commandName {
	case "create", "dropDatabase":
		return []string{database}, nil
	case "drop", "collMod":
		if operation.SourceUUID == "" {
			return []string{database}, nil
		}
		return a.databaseForSourceUUID(ctx, operation.SourceUUID, operation.Namespace)
	case "renameCollection", "createIndexes", "startIndexBuild", "commitIndexBuild", "abortIndexBuild", "dropIndexes":
		return a.databaseForSourceUUID(ctx, operation.SourceUUID, operation.Namespace)
	default:
		return nil, fmt.Errorf("%w command %q at %q", ErrUnsupportedOplogOperation, commandName, operation.Namespace)
	}
}

func (a *Applier) databaseForSourceUUID(ctx context.Context, sourceUUID, namespace string) ([]string, error) {
	if sourceUUID == "" {
		return nil, errors.New("replicated operation has no collection UUID")
	}
	location, err := a.catalog.Resolve(ctx, sourceUUID)
	if errors.Is(err, catalog.ErrSourceUUIDNotFound) {
		database, _, splitErr := splitNamespace(namespace)
		if splitErr != nil {
			return nil, splitErr
		}
		return []string{database}, nil
	}
	if err != nil {
		return nil, err
	}
	return []string{location.Database}, nil
}

func (a *Applier) affectedTransactionDatabases(
	ctx context.Context,
	entry Entry,
	document *types.Document,
	outer operation,
	commandName string,
) ([]string, error) {
	key, hasTransaction, err := transactionKey(document)
	if err != nil {
		return nil, err
	}
	switch commandName {
	case "abortTransaction":
		return nil, nil
	case "commitTransaction":
		if !hasTransaction {
			return nil, errors.New("commitTransaction oplog entry has no session and transaction number")
		}
		fragment, ok := a.store.Snapshot().TransactionParts[key]
		if !ok {
			return nil, fmt.Errorf("commitTransaction has no prepared transaction fragment for %q", key)
		}
		entries, err := decodeTransactionPayload(fragment.Payload)
		if err != nil {
			return nil, err
		}
		return a.affectedTransactionOperations(ctx, entries)
	case "applyOps":
		partial, err := booleanField(outer.Object, "partialTxn")
		if err != nil {
			return nil, err
		}
		prepare, err := booleanField(outer.Object, "prepare")
		if err != nil {
			return nil, err
		}
		fragment, hasFragment := a.store.Snapshot().TransactionParts[key]
		if partial || prepare {
			return nil, nil
		}
		entries := [][]byte{entry.RawBSON}
		if hasFragment {
			payload := append([]byte(nil), fragment.Payload...)
			payload = append(payload, encodeTransactionPayload(entry.RawBSON)...)
			entries, err = decodeTransactionPayload(payload)
			if err != nil {
				return nil, err
			}
		}
		return a.affectedTransactionOperations(ctx, entries)
	default:
		panic("unreachable transaction command")
	}
}

func (a *Applier) affectedTransactionOperations(ctx context.Context, entries [][]byte) ([]string, error) {
	var databases []string
	for _, raw := range entries {
		document, err := bson.ToDocument(wirebson.RawDocument(raw))
		if err != nil {
			return nil, fmt.Errorf("decoding transaction fragment: %w", err)
		}
		objectValue, _ := document.Get("o")
		object, ok := objectValue.(*types.Document)
		if !ok {
			return nil, fmt.Errorf("transaction fragment o has type %T, want document", objectValue)
		}
		arrayValue, _ := object.Get("applyOps")
		array, ok := arrayValue.(*types.Array)
		if !ok {
			return nil, fmt.Errorf("transaction fragment applyOps has type %T, want array", arrayValue)
		}
		iter := array.Iterator()
		for {
			_, value, err := iter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}
			if err != nil {
				iter.Close()
				return nil, err
			}
			document, ok := value.(*types.Document)
			if !ok {
				iter.Close()
				return nil, fmt.Errorf("applyOps element has type %T, want document", value)
			}
			operation, err := parseOperation(document)
			if err != nil {
				iter.Close()
				return nil, err
			}
			affected, err := a.affectedOperationDatabases(ctx, operation)
			if err != nil {
				iter.Close()
				return nil, err
			}
			databases = append(databases, affected...)
		}
		iter.Close()
	}
	slices.Sort(databases)
	return slices.Compact(databases), nil
}
