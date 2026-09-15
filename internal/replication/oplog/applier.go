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
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// ErrUnsupportedOplogOperation stops replication before an effect is silently lost.
var ErrUnsupportedOplogOperation = errors.New("unsupported oplog operation")

// Applier translates source oplog entries into local Prolly mutations.
type Applier struct {
	backend backends.Backend
	catalog *catalog.Applier
	store   *control.Store
}

// NewApplier binds oplog application to a backend and its replication control state.
func NewApplier(backend backends.Backend, store *control.Store) (*Applier, error) {
	if backend == nil || store == nil {
		return nil, errors.New("oplog applier requires backend and control store")
	}
	catalogApplier, err := catalog.NewApplier(backend, store)
	if err != nil {
		return nil, err
	}
	return &Applier{backend: backend, catalog: catalogApplier, store: store}, nil
}

// Apply applies one fetched oplog entry or durably stages its transaction fragment.
func (a *Applier) Apply(ctx context.Context, entry Entry) error {
	document, err := bson.ToDocument(wirebson.RawDocument(entry.RawBSON))
	if err != nil {
		return fmt.Errorf("decoding oplog entry at %v: %w", entry.OpTime, err)
	}
	operation, err := parseOperation(document)
	if err != nil {
		return fmt.Errorf("parsing oplog entry at %v: %w", entry.OpTime, err)
	}
	if operation.Kind != entry.Operation || operation.Namespace != entry.Namespace {
		return fmt.Errorf("oplog entry fields disagree with parsed raw BSON")
	}
	if operation.Kind == "c" {
		commandName, _, err := firstField(operation.Object)
		if err != nil {
			return err
		}
		if commandName == "applyOps" || commandName == "commitTransaction" || commandName == "abortTransaction" {
			return a.applyTransactionEntry(ctx, entry, document, operation, commandName)
		}
	}
	return a.applyOperation(ctx, operation, entry.OpTime)
}

type operation struct {
	Kind       string
	Namespace  string
	SourceUUID string
	Object     *types.Document
	Object2    *types.Document
}

func parseOperation(document *types.Document) (operation, error) {
	kindValue, _ := document.Get("op")
	kind, ok := kindValue.(string)
	if !ok || kind == "" {
		return operation{}, fmt.Errorf("op has type %T, want non-empty string", kindValue)
	}
	namespaceValue, _ := document.Get("ns")
	namespace, ok := namespaceValue.(string)
	if !ok {
		return operation{}, fmt.Errorf("ns has type %T, want string", namespaceValue)
	}
	objectValue, _ := document.Get("o")
	object, ok := objectValue.(*types.Document)
	if !ok {
		return operation{}, fmt.Errorf("o has type %T, want document", objectValue)
	}
	var object2 *types.Document
	if object2Value, getErr := document.Get("o2"); getErr == nil {
		object2, ok = object2Value.(*types.Document)
		if !ok {
			return operation{}, fmt.Errorf("o2 has type %T, want document", object2Value)
		}
	}
	sourceUUID, err := parseSourceUUID(document)
	if err != nil {
		return operation{}, err
	}
	return operation{Kind: kind, Namespace: namespace, SourceUUID: sourceUUID, Object: object, Object2: object2}, nil
}

func parseSourceUUID(document *types.Document) (string, error) {
	value, getErr := document.Get("ui")
	if getErr != nil {
		return "", nil
	}
	binary, ok := value.(types.Binary)
	if !ok || binary.Subtype != types.BinaryUUID || len(binary.B) != 16 {
		return "", fmt.Errorf("ui has type %T, want UUID binary", value)
	}
	parsed, err := uuid.FromBytes(binary.B)
	if err != nil {
		return "", fmt.Errorf("parsing ui: %w", err)
	}
	return parsed.String(), nil
}

func (a *Applier) applyOperation(ctx context.Context, operation operation, opTime control.OpTime) error {
	switch operation.Kind {
	case "n":
		return nil
	case "i", "u", "d":
		if operation.SourceUUID == "" {
			return fmt.Errorf("%s operation for %q has no collection UUID", operation.Kind, operation.Namespace)
		}
		location, err := a.catalog.Resolve(ctx, operation.SourceUUID)
		if err != nil {
			return err
		}
		database, err := a.backend.Database(replicationDatabaseName(location.Database, a.store.Snapshot().Configuration.Branch))
		if err != nil {
			return err
		}
		collection, err := database.Collection(location.Collection)
		if err != nil {
			return err
		}
		switch operation.Kind {
		case "i":
			return applyInsert(ctx, collection, operation.Object)
		case "u":
			return applyUpdate(ctx, collection, operation.Object, operation.Object2)
		default:
			return applyDelete(ctx, collection, operation.Object)
		}
	case "c":
		return a.applyCommand(ctx, operation, opTime)
	default:
		return fmt.Errorf("%w %q at %q", ErrUnsupportedOplogOperation, operation.Kind, operation.Namespace)
	}
}

func (a *Applier) applyCommand(ctx context.Context, operation operation, opTime control.OpTime) error {
	databaseName, collectionName, err := splitNamespace(operation.Namespace)
	if err != nil {
		return err
	}
	if collectionName != "$cmd" {
		return fmt.Errorf("command oplog namespace %q does not end in .$cmd", operation.Namespace)
	}
	commandName, commandValue, err := firstField(operation.Object)
	if err != nil {
		return err
	}
	switch commandName {
	case "create":
		name, ok := commandValue.(string)
		if !ok || name == "" {
			return fmt.Errorf("create command target has type %T, want non-empty string", commandValue)
		}
		options, err := documentWithout(operation.Object, "create", "idIndex")
		if err != nil {
			return err
		}
		var indexes []*types.Document
		if idIndexValue, idIndexErr := operation.Object.Get("idIndex"); idIndexErr == nil {
			idIndex, ok := idIndexValue.(*types.Document)
			if !ok {
				return fmt.Errorf("create idIndex has type %T, want document", idIndexValue)
			}
			indexes = append(indexes, idIndex)
		}
		plan, err := catalog.PreflightCollection(databaseName, name, options, indexes)
		if err != nil {
			return err
		}
		if plan.Create.ViewOn != "" {
			if operation.SourceUUID != "" {
				return errors.New("create view unexpectedly has a collection UUID")
			}
			return a.catalog.CreateView(ctx, databaseName, plan.Create)
		}
		if operation.SourceUUID == "" {
			return errors.New("create collection has no collection UUID")
		}
		if _, err := a.catalog.Create(ctx, databaseName, plan.Create, operation.SourceUUID, opTime); err != nil {
			return err
		}
		return a.catalog.CreateIndexes(ctx, operation.SourceUUID, plan.Indexes)
	case "drop":
		name, ok := commandValue.(string)
		if !ok || name == "" {
			return fmt.Errorf("drop command target has type %T, want non-empty string", commandValue)
		}
		if operation.SourceUUID == "" {
			return a.catalog.DropView(ctx, databaseName, name)
		}
		return a.catalog.Drop(ctx, operation.SourceUUID, opTime)
	case "renameCollection":
		if operation.SourceUUID == "" {
			return errors.New("renameCollection has no collection UUID")
		}
		targetValue, targetErr := operation.Object.Get("to")
		target, ok := targetValue.(string)
		if targetErr != nil || !ok {
			return fmt.Errorf("renameCollection to has type %T, want string", targetValue)
		}
		targetDatabase, targetCollection, err := splitNamespace(target)
		if err != nil {
			return err
		}
		return a.catalog.Rename(ctx, operation.SourceUUID, targetDatabase, targetCollection, opTime)
	case "createIndexes":
		if operation.SourceUUID == "" {
			return errors.New("createIndexes has no collection UUID")
		}
		name, ok := commandValue.(string)
		if !ok || name == "" {
			return fmt.Errorf("createIndexes target has type %T, want non-empty string", commandValue)
		}
		indexDocument, err := documentWithout(operation.Object, "createIndexes")
		if err != nil {
			return err
		}
		indexes, err := catalog.PreflightIndexes(databaseName, name, []*types.Document{indexDocument})
		if err != nil {
			return err
		}
		return a.catalog.CreateIndexes(ctx, operation.SourceUUID, indexes)
	case "dropIndexes":
		if operation.SourceUUID == "" {
			return errors.New("dropIndexes has no collection UUID")
		}
		indexValue, indexErr := operation.Object.Get("index")
		index, ok := indexValue.(string)
		if indexErr != nil || !ok || index == "" {
			return fmt.Errorf("dropIndexes index has type %T, want non-empty string", indexValue)
		}
		return a.catalog.DropIndexes(ctx, operation.SourceUUID, []string{index})
	case "collMod":
		name, ok := commandValue.(string)
		if !ok || name == "" {
			return fmt.Errorf("collMod target has type %T, want non-empty string", commandValue)
		}
		params, err := parseCollMod(operation.Object, name)
		if err != nil {
			return err
		}
		if operation.SourceUUID == "" {
			return a.catalog.CollModView(ctx, databaseName, params)
		}
		return a.catalog.CollMod(ctx, operation.SourceUUID, params)
	case "dropDatabase":
		return a.catalog.DropDatabase(ctx, databaseName, opTime)
	default:
		return fmt.Errorf("%w command %q at %q", ErrUnsupportedOplogOperation, commandName, operation.Namespace)
	}
}

func (a *Applier) applyTransactionEntry(ctx context.Context, entry Entry, document *types.Document, outer operation, commandName string) error {
	key, hasTransaction, err := transactionKey(document)
	if err != nil {
		return err
	}
	switch commandName {
	case "abortTransaction":
		if !hasTransaction {
			return errors.New("abortTransaction oplog entry has no session and transaction number")
		}
		fragment, ok := a.store.Snapshot().TransactionParts[key]
		if !ok {
			return fmt.Errorf("abortTransaction has no transaction fragment for %q", key)
		}
		previous, err := previousOpTime(document)
		if err != nil {
			return err
		}
		if previous != fragment.Last {
			return fmt.Errorf("abortTransaction prevOpTime %v does not identify transaction fragment %v", previous, fragment.Last)
		}
		return a.store.DeleteTransactionFragment(key)
	case "commitTransaction":
		if !hasTransaction {
			return errors.New("commitTransaction oplog entry has no session and transaction number")
		}
		fragment, ok := a.store.Snapshot().TransactionParts[key]
		if !ok {
			return fmt.Errorf("commitTransaction has no prepared transaction fragment for %q", key)
		}
		previous, err := previousOpTime(document)
		if err != nil {
			return err
		}
		if previous != fragment.Last {
			return fmt.Errorf("commitTransaction prevOpTime %v does not identify prepared fragment %v", previous, fragment.Last)
		}
		entries, err := decodeTransactionPayload(fragment.Payload)
		if err != nil {
			return err
		}
		if err := a.applyTransactionOperations(ctx, entries, entry.OpTime); err != nil {
			return err
		}
		return a.store.DeleteTransactionFragment(key)
	case "applyOps":
		partial, err := booleanField(outer.Object, "partialTxn")
		if err != nil {
			return err
		}
		prepare, err := booleanField(outer.Object, "prepare")
		if err != nil {
			return err
		}
		if partial || prepare || hasTransaction {
			if !hasTransaction {
				return errors.New("transactional applyOps has no session and transaction number")
			}
			fragment, exists := a.store.Snapshot().TransactionParts[key]
			if err := validateTransactionLink(document, fragment, exists); err != nil {
				return err
			}
			payload := encodeTransactionPayload(entry.RawBSON)
			first := entry.OpTime
			if exists {
				payload = append(fragment.Payload, payload...)
				first = fragment.First
			}
			fragment = control.TransactionFragment{Key: key, First: first, Last: entry.OpTime, Payload: payload}
			if partial || prepare {
				return a.store.PutTransactionFragment(fragment)
			}
			entries, err := decodeTransactionPayload(fragment.Payload)
			if err != nil {
				return err
			}
			if err := a.applyTransactionOperations(ctx, entries, entry.OpTime); err != nil {
				return err
			}
			return a.store.DeleteTransactionFragment(key)
		}
		return a.applyTransactionOperations(ctx, [][]byte{entry.RawBSON}, entry.OpTime)
	default:
		panic("unreachable transaction command")
	}
}

func (a *Applier) applyTransactionOperations(ctx context.Context, entries [][]byte, opTime control.OpTime) error {
	operations := make([]operation, 0)
	for _, raw := range entries {
		document, err := bson.ToDocument(wirebson.RawDocument(raw))
		if err != nil {
			return fmt.Errorf("decoding transaction fragment: %w", err)
		}
		objectValue, _ := document.Get("o")
		object, ok := objectValue.(*types.Document)
		if !ok {
			return fmt.Errorf("transaction fragment o has type %T, want document", objectValue)
		}
		arrayValue, _ := object.Get("applyOps")
		array, ok := arrayValue.(*types.Array)
		if !ok {
			return fmt.Errorf("transaction fragment applyOps has type %T, want array", arrayValue)
		}
		iter := array.Iterator()
		for {
			_, value, err := iter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}
			if err != nil {
				iter.Close()
				return err
			}
			document, ok := value.(*types.Document)
			if !ok {
				iter.Close()
				return fmt.Errorf("applyOps element has type %T, want document", value)
			}
			operation, err := parseOperation(document)
			if err != nil {
				iter.Close()
				return err
			}
			if operation.Kind == "c" {
				iter.Close()
				return errors.New("catalog commands inside replicated transactions are not supported")
			}
			operations = append(operations, operation)
		}
		iter.Close()
	}
	return a.withAtomicVisibility(ctx, func(transactionContext context.Context) error {
		for _, operation := range operations {
			if err := a.applyOperation(transactionContext, operation, opTime); err != nil {
				return err
			}
		}
		return nil
	})
}

func (a *Applier) withAtomicVisibility(ctx context.Context, apply func(context.Context) error) error {
	sessionBackend, ok := a.backend.(backends.SessionAwareBackend)
	if !ok || sessionBackend.SessionRegistry() == nil {
		return errors.New("replicated transaction requires a session-aware backend")
	}
	connectionInfo := conninfo.New()
	connectionInfo.SetLSID("replication:" + uuid.NewString())
	connectionInfo.SetInTransaction(true)
	owner := connectionInfo.Owner()
	shadow, err := sessionBackend.SessionRegistry().Connect(owner)
	if err != nil {
		return err
	}
	connectionInfo.SetCachedShadow(owner, shadow)
	transactionContext := conninfo.Ctx(ctx, connectionInfo)
	defer sessionBackend.OnSessionEnd(owner)
	err = shadow.Commit(time.Now(), func(session *dsess.DoltSession) error {
		sqlContext := sqlctx.Wrap(transactionContext, session)
		if _, err := sqlctx.EnsureTxn(sqlContext, session); err != nil {
			return err
		}
		if err := apply(transactionContext); err != nil {
			sessionBackend.OnTransactionAbort(owner)
			return err
		}
		if err := sessionBackend.OnTransactionCommit(transactionContext, owner); err != nil {
			sessionBackend.OnTransactionAbort(owner)
			return err
		}
		connectionInfo.SetInTransaction(false)
		return nil
	})
	if err != nil {
		sessionBackend.OnTransactionAbort(owner)
	}
	return err
}

func transactionKey(document *types.Document) (string, bool, error) {
	lsidValue, lsidErr := document.Get("lsid")
	txnValue, txnErr := document.Get("txnNumber")
	if lsidErr != nil && txnErr != nil {
		return "", false, nil
	}
	if lsidErr != nil || txnErr != nil {
		return "", false, errors.New("oplog transaction identity requires both lsid and txnNumber")
	}
	lsid, ok := lsidValue.(*types.Document)
	if !ok {
		return "", false, fmt.Errorf("lsid has type %T, want document", lsidValue)
	}
	raw, err := bson.FromDocumentRaw(lsid)
	if err != nil {
		return "", false, err
	}
	txnNumber, err := entryInteger(txnValue)
	if err != nil {
		return "", false, fmt.Errorf("txnNumber: %w", err)
	}
	return hex.EncodeToString(raw) + ":" + fmt.Sprint(txnNumber), true, nil
}

func validateTransactionLink(document *types.Document, fragment control.TransactionFragment, exists bool) error {
	previous, err := previousOpTime(document)
	if err != nil {
		return err
	}
	if !exists {
		if previous.Seconds != 0 || previous.Increment != 0 || previous.Term != -1 {
			return fmt.Errorf("first transaction fragment has non-null prevOpTime %v", previous)
		}
		return nil
	}
	if previous != fragment.Last {
		return fmt.Errorf("transaction fragment prevOpTime %v does not identify prior fragment %v", previous, fragment.Last)
	}
	return nil
}

func previousOpTime(document *types.Document) (control.OpTime, error) {
	value, err := document.Get("prevOpTime")
	if err != nil {
		return control.OpTime{}, errors.New("transaction oplog entry has no prevOpTime")
	}
	opTime, ok := value.(*types.Document)
	if !ok {
		return control.OpTime{}, fmt.Errorf("prevOpTime has type %T, want document", value)
	}
	timestampValue, _ := opTime.Get("ts")
	timestamp, ok := timestampValue.(types.Timestamp)
	if !ok {
		return control.OpTime{}, fmt.Errorf("prevOpTime.ts has type %T, want timestamp", timestampValue)
	}
	termValue, _ := opTime.Get("t")
	term, err := entryInteger(termValue)
	if err != nil {
		return control.OpTime{}, fmt.Errorf("prevOpTime.t: %w", err)
	}
	return control.OpTime{Seconds: uint32(uint64(timestamp) >> 32), Increment: uint32(timestamp), Term: term}, nil
}

func booleanField(document *types.Document, field string) (bool, error) {
	value, err := document.Get(field)
	if err != nil {
		return false, nil
	}
	result, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s has type %T, want boolean", field, value)
	}
	return result, nil
}

func encodeTransactionPayload(raw []byte) []byte {
	payload := make([]byte, 4+len(raw))
	binary.LittleEndian.PutUint32(payload, uint32(len(raw)))
	copy(payload[4:], raw)
	return payload
}

func decodeTransactionPayload(payload []byte) ([][]byte, error) {
	var entries [][]byte
	for len(payload) > 0 {
		if len(payload) < 4 {
			return nil, errors.New("truncated transaction fragment length")
		}
		length := int(binary.LittleEndian.Uint32(payload))
		payload = payload[4:]
		if length < 5 || length > len(payload) {
			return nil, fmt.Errorf("invalid transaction fragment length %d", length)
		}
		entries = append(entries, append([]byte(nil), payload[:length]...))
		payload = payload[length:]
	}
	return entries, nil
}

func parseCollMod(document *types.Document, name string) (backends.CollModParams, error) {
	params := backends.CollModParams{Name: name}
	iter := document.Iterator()
	defer iter.Close()
	for {
		field, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return params, nil
		}
		if err != nil {
			return backends.CollModParams{}, err
		}
		switch field {
		case "collMod":
		case "validator":
			params.Validator, _ = value.(*types.Document)
			if params.Validator == nil {
				return backends.CollModParams{}, fmt.Errorf("collMod validator has type %T, want document", value)
			}
			params.SetValidator = true
		case "validationLevel":
			params.ValidationLevel, _ = value.(string)
			if params.ValidationLevel == "" {
				return backends.CollModParams{}, fmt.Errorf("collMod validationLevel has type %T, want string", value)
			}
		case "validationAction":
			params.ValidationAction, _ = value.(string)
			if params.ValidationAction == "" {
				return backends.CollModParams{}, fmt.Errorf("collMod validationAction has type %T, want string", value)
			}
		case "viewOn":
			params.ViewOn, _ = value.(string)
			if params.ViewOn == "" {
				return backends.CollModParams{}, fmt.Errorf("collMod viewOn has type %T, want string", value)
			}
			params.SetView, params.SetViewOn = true, true
		case "pipeline":
			params.ViewPipeline, _ = value.(*types.Array)
			if params.ViewPipeline == nil {
				return backends.CollModParams{}, fmt.Errorf("collMod pipeline has type %T, want array", value)
			}
			params.SetView, params.SetViewPipeline = true, true
		default:
			return backends.CollModParams{}, fmt.Errorf("%w collMod option %q", ErrUnsupportedOplogOperation, field)
		}
	}
}

func firstField(document *types.Document) (string, any, error) {
	iter := document.Iterator()
	defer iter.Close()
	field, value, err := iter.Next()
	if errors.Is(err, iterator.ErrIteratorDone) {
		return "", nil, errors.New("empty oplog command")
	}
	return field, value, err
}

func documentWithout(document *types.Document, excluded ...string) (*types.Document, error) {
	skip := make(map[string]struct{}, len(excluded))
	for _, field := range excluded {
		skip[field] = struct{}{}
	}
	pairs := make([]any, 0, document.Len()*2)
	iter := document.Iterator()
	defer iter.Close()
	for {
		field, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return types.NewDocument(pairs...)
		}
		if err != nil {
			return nil, err
		}
		if _, excluded := skip[field]; excluded {
			continue
		}
		pairs = append(pairs, field, value)
	}
}

func applyInsert(ctx context.Context, collection backends.Collection, document *types.Document) error {
	id, getErr := document.Get("_id")
	if getErr != nil {
		return errors.New("insert oplog document has no _id")
	}
	existing, found, err := findDocumentByID(ctx, collection, id)
	if err != nil {
		return err
	}
	if found {
		if types.Compare(existing, document) == types.Equal {
			return nil
		}
		return fmt.Errorf("insert oplog document conflicts with existing _id %v", id)
	}
	_, err = collection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{document.DeepCopy()}})
	return err
}

func applyUpdate(ctx context.Context, collection backends.Collection, update, criteria *types.Document) error {
	if criteria == nil {
		return errors.New("update oplog entry has no o2 document key")
	}
	id, getErr := criteria.Get("_id")
	if getErr != nil {
		return errors.New("update oplog document key has no _id")
	}
	existing, found, err := findDocumentByID(ctx, collection, id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("update oplog target _id %v does not exist", id)
	}
	postImage := update
	if versionValue, versionErr := update.Get("$v"); versionErr == nil {
		version, err := entryInteger(versionValue)
		if err != nil || version != 2 {
			return fmt.Errorf("unsupported oplog update version %v", versionValue)
		}
		diffValue, diffErr := update.Get("diff")
		diff, valid := diffValue.(*types.Document)
		if diffErr != nil || !valid {
			return fmt.Errorf("version 2 update diff has type %T, want document", diffValue)
		}
		postImage, err = ApplyV2Diff(existing, diff)
		if err != nil {
			return err
		}
	}
	postID, postIDErr := postImage.Get("_id")
	if postIDErr != nil || types.Compare(id, postID) != types.Equal {
		return errors.New("update oplog post-image changes or omits _id")
	}
	if types.Compare(existing, postImage) == types.Equal {
		return nil
	}
	result, err := collection.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{postImage.DeepCopy()}})
	if err != nil {
		return err
	}
	if result.Updated != 1 {
		return fmt.Errorf("update oplog target _id %v matched %d documents", id, result.Updated)
	}
	return nil
}

func applyDelete(ctx context.Context, collection backends.Collection, documentKey *types.Document) error {
	id, getErr := documentKey.Get("_id")
	if getErr != nil {
		return errors.New("delete oplog document key has no _id")
	}
	_, found, err := findDocumentByID(ctx, collection, id)
	if err != nil || !found {
		return err
	}
	result, err := collection.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{id}})
	if err != nil {
		return err
	}
	if result.Deleted != 1 {
		return fmt.Errorf("delete oplog target _id %v matched %d documents", id, result.Deleted)
	}
	return nil
}

func findDocumentByID(ctx context.Context, collection backends.Collection, id any) (*types.Document, bool, error) {
	filter, err := types.NewDocument("_id", id)
	if err != nil {
		return nil, false, err
	}
	result, err := collection.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return nil, false, err
	}
	defer result.Iter.Close()
	for {
		_, document, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		candidate, candidateErr := document.Get("_id")
		if candidateErr == nil && types.Compare(candidate, id) == types.Equal {
			return document, true, nil
		}
	}
}

func splitNamespace(namespace string) (string, string, error) {
	dot := strings.IndexByte(namespace, '.')
	if dot <= 0 || dot == len(namespace)-1 {
		return "", "", fmt.Errorf("invalid namespace %q", namespace)
	}
	return namespace[:dot], namespace[dot+1:], nil
}

func replicationDatabaseName(database, branch string) string {
	if branch == "main" {
		return database
	}
	return database + "@" + branch
}
