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

package special

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

const (
	UsersNamespace                 = "admin.system.users"
	RolesNamespace                 = "admin.system.roles"
	TransactionsNamespace          = "config.transactions"
	RetryImagesNamespace           = "config.image_collection"
	ChangeStreamPreimagesNamespace = "config.system.preimages"
	IndexBuildsNamespace           = "config.system.indexBuilds"
)

var ErrUnsupportedSpecialNamespace = errors.New("unsupported replicated special namespace")

type Applier struct {
	backend            backends.Backend
	store              *control.Store
	owner              string
	bumpAuthGeneration func()
}

func NewApplier(backend backends.Backend, store *control.Store, bumpAuthGeneration func()) (*Applier, error) {
	if backend == nil || store == nil {
		return nil, errors.New("special namespace applier requires backend and control store")
	}
	owner := store.Snapshot().Configuration.SetName
	if owner == "" {
		return nil, errors.New("special namespace applier requires a replication owner")
	}
	return &Applier{backend: backend, store: store, owner: owner, bumpAuthGeneration: bumpAuthGeneration}, nil
}

func IsAuthNamespace(namespace string) bool {
	return namespace == UsersNamespace || namespace == RolesNamespace
}

func MetadataKind(namespace string) (control.MetadataKind, bool) {
	switch namespace {
	case TransactionsNamespace:
		return control.MetadataTransaction, true
	case RetryImagesNamespace:
		return control.MetadataRetryImage, true
	default:
		return "", false
	}
}

func IsSpecialNamespace(namespace string) bool {
	_, metadata := MetadataKind(namespace)
	return IsAuthNamespace(namespace) || metadata || namespace == ChangeStreamPreimagesNamespace
}

// IsIgnoredNamespace identifies source-local metadata that is not replicated application state.
func IsIgnoredNamespace(namespace string) bool {
	switch namespace {
	case "admin.system.version", "admin.system.keys",
		"config.system.sessions", IndexBuildsNamespace,
		"config.analyzeShardKeySplitPoints", "config.sampledQueries", "config.sampledQueriesDiff":
		return true
	default:
		return false
	}
}

func (a *Applier) ApplyInitialDocument(ctx context.Context, namespace string, document *types.Document, opTime control.OpTime) error {
	switch {
	case IsAuthNamespace(namespace):
		return a.InsertAuthDocument(ctx, namespace, document, opTime)
	case namespace == ChangeStreamPreimagesNamespace:
		return fmt.Errorf("%w %q: change-stream pre-images are not supported", ErrUnsupportedSpecialNamespace, namespace)
	default:
		if _, ok := MetadataKind(namespace); !ok {
			return fmt.Errorf("%w %q", ErrUnsupportedSpecialNamespace, namespace)
		}
		return a.PutMetadataDocument(namespace, document, opTime)
	}
}

func (a *Applier) InsertAuthDocument(ctx context.Context, namespace string, document *types.Document, opTime control.OpTime) error {
	translated, identity, err := a.translateAuth(namespace, document)
	if err != nil {
		return err
	}
	collection, err := a.authCollection(namespace)
	if err != nil {
		return err
	}
	existing, found, err := findDocumentByID(ctx, collection, identity)
	if err != nil {
		return err
	}
	ownership, owned := a.store.AuthOwnershipFor(namespace, identity)
	if found {
		if !owned || ownership.Owner != a.owner || ownership.Dropped {
			return fmt.Errorf("replicated identity %q conflicts with a DumboDB-local administrator", identity)
		}
		if !authValuesEqual(existing, translated.Document) {
			return fmt.Errorf("replicated identity %q conflicts with its owned document", identity)
		}
	}
	if owned && !ownership.Dropped && ownership.Owner != a.owner {
		return fmt.Errorf("replicated identity %q is owned by %q", identity, ownership.Owner)
	}
	record := control.AuthOwnership{Namespace: namespace, Identity: identity, Owner: a.owner, LastUpdateOpTime: opTime}
	if err := a.store.PutAuthOwnership(record); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := collection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{translated.Document}}); err != nil {
		return err
	}
	a.bumpAuth()
	return nil
}

func (a *Applier) ReplaceAuthDocument(ctx context.Context, namespace string, document *types.Document, opTime control.OpTime) error {
	translated, identity, err := a.translateAuth(namespace, document)
	if err != nil {
		return err
	}
	ownership, owned := a.store.AuthOwnershipFor(namespace, identity)
	if !owned || ownership.Dropped || ownership.Owner != a.owner {
		return fmt.Errorf("replicated identity %q is not owned by replica set %q", identity, a.owner)
	}
	collection, err := a.authCollection(namespace)
	if err != nil {
		return err
	}
	existing, found, err := findDocumentByID(ctx, collection, identity)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("replicated identity %q does not exist", identity)
	}
	record := control.AuthOwnership{Namespace: namespace, Identity: identity, Owner: a.owner, LastUpdateOpTime: opTime}
	changed := !authValuesEqual(existing, translated.Document)
	if changed {
		result, err := collection.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{translated.Document}})
		if err != nil {
			return err
		}
		if result.Updated != 1 {
			return fmt.Errorf("replicated identity %q matched %d documents", identity, result.Updated)
		}
	}
	if err := a.store.PutAuthOwnership(record); err != nil {
		return err
	}
	if changed {
		a.bumpAuth()
	}
	return nil
}

func (a *Applier) DeleteAuthDocument(ctx context.Context, namespace string, documentKey *types.Document, opTime control.OpTime) error {
	identity, err := stringID(documentKey)
	if err != nil {
		return err
	}
	ownership, owned := a.store.AuthOwnershipFor(namespace, identity)
	if owned && ownership.Owner != a.owner {
		return fmt.Errorf("replicated identity %q is not owned by replica set %q", identity, a.owner)
	}
	if owned && ownership.Dropped {
		return a.store.DropAuthOwnership(namespace, identity, a.owner, opTime)
	}
	collection, err := a.authCollection(namespace)
	if err != nil {
		return err
	}
	_, found, err := findDocumentByID(ctx, collection, identity)
	if err != nil {
		return err
	}
	if found && owned {
		result, err := collection.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{identity}})
		if err != nil {
			return err
		}
		if result.Deleted != 1 {
			return fmt.Errorf("replicated identity %q matched %d documents", identity, result.Deleted)
		}
	}
	if err := a.store.DropAuthOwnership(namespace, identity, a.owner, opTime); err != nil {
		return err
	}
	if owned {
		a.bumpAuth()
	}
	return nil
}

func (a *Applier) AuthDocument(ctx context.Context, namespace string, documentKey *types.Document) (*types.Document, error) {
	identity, err := stringID(documentKey)
	if err != nil {
		return nil, err
	}
	ownership, owned := a.store.AuthOwnershipFor(namespace, identity)
	if !owned || ownership.Dropped || ownership.Owner != a.owner {
		return nil, fmt.Errorf("replicated identity %q is not owned by replica set %q", identity, a.owner)
	}
	collection, err := a.authCollection(namespace)
	if err != nil {
		return nil, err
	}
	document, found, err := findDocumentByID(ctx, collection, identity)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("replicated identity %q does not exist", identity)
	}
	return document, nil
}

func (a *Applier) PutMetadataDocument(namespace string, document *types.Document, opTime control.OpTime) error {
	kind, ok := MetadataKind(namespace)
	if !ok {
		return fmt.Errorf("%w %q", ErrUnsupportedSpecialNamespace, namespace)
	}
	if err := validateMetadataDocument(kind, document); err != nil {
		return fmt.Errorf("replicated %s metadata: %w", kind, err)
	}
	key, err := metadataKey(document)
	if err != nil {
		return err
	}
	raw, err := bson.FromDocumentRaw(document)
	if err != nil {
		return err
	}
	return a.store.PutReplicationMetadata(control.ReplicationMetadataRecord{
		Kind: kind, Namespace: namespace, Key: key, Document: raw, LastUpdateOpTime: opTime,
	})
}

func (a *Applier) MetadataDocument(namespace string, documentKey *types.Document) (*types.Document, error) {
	kind, ok := MetadataKind(namespace)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnsupportedSpecialNamespace, namespace)
	}
	key, err := metadataKey(documentKey)
	if err != nil {
		return nil, err
	}
	record, found := a.store.ReplicationMetadataRecordFor(kind, key)
	if !found || record.Deleted {
		return nil, fmt.Errorf("replication metadata %q does not exist", key)
	}
	return bson.ToDocument(wirebson.RawDocument(record.Document))
}

func (a *Applier) DeleteMetadataDocument(namespace string, documentKey *types.Document, opTime control.OpTime) error {
	kind, ok := MetadataKind(namespace)
	if !ok {
		return fmt.Errorf("%w %q", ErrUnsupportedSpecialNamespace, namespace)
	}
	key, err := metadataKey(documentKey)
	if err != nil {
		return err
	}
	return a.store.DeleteReplicationMetadata(kind, key, opTime)
}

func (a *Applier) translateAuth(namespace string, document *types.Document) (AuthDocument, string, error) {
	var translated AuthDocument
	var err error
	switch namespace {
	case UsersNamespace:
		translated, err = TranslateUser(document, a.owner)
	case RolesNamespace:
		translated, err = TranslateRole(document, a.owner)
	default:
		return AuthDocument{}, "", fmt.Errorf("%w %q", ErrUnsupportedSpecialNamespace, namespace)
	}
	if err != nil {
		return AuthDocument{}, "", err
	}
	identity, err := stringID(translated.Document)
	return translated, identity, err
}

func (a *Applier) authCollection(namespace string) (backends.Collection, error) {
	database, err := a.backend.Database("admin")
	if err != nil {
		return nil, err
	}
	collectionName := ""
	switch namespace {
	case UsersNamespace:
		collectionName = "system.users"
	case RolesNamespace:
		collectionName = "system.roles"
	default:
		return nil, fmt.Errorf("%w %q", ErrUnsupportedSpecialNamespace, namespace)
	}
	return database.Collection(collectionName)
}

func (a *Applier) bumpAuth() {
	if a.bumpAuthGeneration != nil {
		a.bumpAuthGeneration()
	}
}

func stringID(document *types.Document) (string, error) {
	if document == nil {
		return "", errors.New("replicated document key is required")
	}
	value, err := document.Get("_id")
	if err != nil {
		return "", errors.New("replicated document has no _id")
	}
	id, ok := value.(string)
	if !ok || id == "" {
		return "", fmt.Errorf("replicated identity _id has type %T, want non-empty string", value)
	}
	return id, nil
}

func metadataKey(document *types.Document) (string, error) {
	if document == nil {
		return "", errors.New("replication metadata document is required")
	}
	id, err := document.Get("_id")
	if err != nil {
		return "", errors.New("replication metadata document has no _id")
	}
	keyDocument := must.NotFail(types.NewDocument("_id", id))
	raw, err := bson.FromDocumentRaw(keyDocument)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func validateMetadataDocument(kind control.MetadataKind, document *types.Document) error {
	if document == nil {
		return errors.New("document is required")
	}
	if err := requireLogicalSessionID(document); err != nil {
		return err
	}
	switch kind {
	case control.MetadataTransaction:
		if err := requireInt64(document, "txnNum"); err != nil {
			return err
		}
		if err := requireOpTime(document, "lastWriteOpTime"); err != nil {
			return err
		}
		if value, err := document.Get("lastWriteDate"); err != nil {
			return errors.New("lastWriteDate is required")
		} else if _, ok := value.(time.Time); !ok {
			return fmt.Errorf("lastWriteDate has type %T, want date", value)
		}
		if value, err := document.Get("state"); err == nil {
			state, ok := value.(string)
			if !ok || state != "prepared" && state != "committed" && state != "aborted" && state != "inProgress" {
				return fmt.Errorf("state %v is not a supported durable transaction state", value)
			}
		}
	case control.MetadataRetryImage:
		if err := requireInt64(document, "txnNum"); err != nil {
			return err
		}
		if value, err := document.Get("ts"); err != nil {
			return errors.New("ts is required")
		} else if _, ok := value.(types.Timestamp); !ok {
			return fmt.Errorf("ts has type %T, want timestamp", value)
		}
		kindValue, err := document.Get("imageKind")
		if err != nil {
			return errors.New("imageKind is required")
		}
		imageKind, ok := kindValue.(string)
		if !ok || imageKind != "preImage" && imageKind != "postImage" {
			return fmt.Errorf("imageKind %v is not preImage or postImage", kindValue)
		}
		if value, err := document.Get("image"); err != nil {
			return errors.New("image is required")
		} else if _, ok := value.(*types.Document); !ok {
			return fmt.Errorf("image has type %T, want document", value)
		}
	}
	return nil
}

func requireLogicalSessionID(document *types.Document) error {
	value, err := document.Get("_id")
	if err != nil {
		return errors.New("_id is required")
	}
	sessionID, ok := value.(*types.Document)
	if !ok {
		return fmt.Errorf("_id has type %T, want logical session document", value)
	}
	for _, field := range sessionID.Keys() {
		if field != "id" && field != "uid" && field != "txnNumber" && field != "txnUUID" {
			return fmt.Errorf("_id.%s is not a logical session field", field)
		}
	}
	idValue, err := sessionID.Get("id")
	if err != nil {
		return errors.New("_id.id is required")
	}
	if id, ok := idValue.(types.Binary); !ok || id.Subtype != types.BinaryUUID || len(id.B) != 16 {
		return errors.New("_id.id must be a UUID")
	}
	uidValue, err := sessionID.Get("uid")
	if err != nil {
		return errors.New("_id.uid is required")
	}
	if uid, ok := uidValue.(types.Binary); !ok || uid.Subtype != types.BinaryGeneric || len(uid.B) != 32 {
		return errors.New("_id.uid must be a 32-byte generic binary value")
	}
	if txnNumber, err := sessionID.Get("txnNumber"); err == nil {
		if _, ok := txnNumber.(int64); !ok {
			return fmt.Errorf("_id.txnNumber has type %T, want int64", txnNumber)
		}
	}
	if txnUUID, err := sessionID.Get("txnUUID"); err == nil {
		if value, ok := txnUUID.(types.Binary); !ok || value.Subtype != types.BinaryUUID || len(value.B) != 16 {
			return errors.New("_id.txnUUID must be a UUID")
		}
	}
	return nil
}

func requireInt64(document *types.Document, field string) error {
	value, err := document.Get(field)
	if err != nil {
		return fmt.Errorf("%s is required", field)
	}
	if _, ok := value.(int64); !ok {
		return fmt.Errorf("%s has type %T, want int64", field, value)
	}
	return nil
}

func requireOpTime(document *types.Document, field string) error {
	value, err := document.Get(field)
	if err != nil {
		return fmt.Errorf("%s is required", field)
	}
	opTime, ok := value.(*types.Document)
	if !ok {
		return fmt.Errorf("%s has type %T, want document", field, value)
	}
	if value, err := opTime.Get("ts"); err != nil {
		return fmt.Errorf("%s.ts is required", field)
	} else if _, ok := value.(types.Timestamp); !ok {
		return fmt.Errorf("%s.ts has type %T, want timestamp", field, value)
	}
	if value, err := opTime.Get("t"); err != nil {
		return fmt.Errorf("%s.t is required", field)
	} else if _, ok := value.(int64); !ok {
		return fmt.Errorf("%s.t has type %T, want int64", field, value)
	}
	return nil
}

func findDocumentByID(ctx context.Context, collection backends.Collection, id any) (*types.Document, bool, error) {
	filter := must.NotFail(types.NewDocument("_id", id))
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

func authValuesEqual(left, right any) bool {
	switch left := left.(type) {
	case *types.Document:
		right, ok := right.(*types.Document)
		if !ok || left.Len() != right.Len() {
			return false
		}
		for _, key := range left.Keys() {
			leftValue, _ := left.Get(key)
			rightValue, err := right.Get(key)
			if err != nil || !authValuesEqual(leftValue, rightValue) {
				return false
			}
		}
		return true
	case *types.Array:
		right, ok := right.(*types.Array)
		if !ok || left.Len() != right.Len() {
			return false
		}
		for index := 0; index < left.Len(); index++ {
			leftValue, _ := left.Get(index)
			rightValue, _ := right.Get(index)
			if !authValuesEqual(leftValue, rightValue) {
				return false
			}
		}
		return true
	default:
		return types.Compare(left, right) == types.Equal
	}
}
