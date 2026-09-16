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

package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dumbodb/internal/backends"
	dumbobson "github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	mongobson "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsonrw"
)

const (
	controlDocumentID          = "control"
	legacyControlFormatVersion = int32(1)
	controlFormatVersion       = int32(2)
)

type collectionStorage struct {
	collection backends.Collection
}

func openCollectionStorage(backend backends.Backend) (*collectionStorage, error) {
	provider, ok := backend.(backends.ReplicationControlBackend)
	if !ok {
		return nil, errors.New("backend does not support replication control storage")
	}
	collection, err := provider.ReplicationControlCollection()
	if err != nil {
		return nil, fmt.Errorf("opening replication control collection: %w", err)
	}
	return &collectionStorage{collection: collection}, nil
}

func (s *collectionStorage) load(ctx context.Context) (State, bool, error) {
	filter, err := types.NewDocument("_id", controlDocumentID)
	if err != nil {
		return State{}, false, err
	}
	result, err := s.collection.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return State{}, false, fmt.Errorf("querying replication control state: %w", err)
	}
	defer result.Iter.Close()
	for {
		_, document, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return State{}, false, nil
		}
		if err != nil {
			return State{}, false, fmt.Errorf("reading replication control state: %w", err)
		}
		id, _ := document.Get("_id")
		if id != controlDocumentID {
			continue
		}
		version, _ := document.Get("formatVersion")
		value, err := document.Get("state")
		if err != nil {
			return State{}, false, errors.New("replication control document has no state")
		}
		switch version {
		case legacyControlFormatVersion:
			binary, ok := value.(types.Binary)
			if !ok || binary.Subtype != types.BinaryGeneric {
				return State{}, false, errors.New("legacy replication control state is not generic binary data")
			}
			var state State
			if err := json.Unmarshal(binary.B, &state); err != nil {
				return State{}, false, fmt.Errorf("decoding legacy replication control state: %w", err)
			}
			return state, true, nil
		case controlFormatVersion:
			stateDocument, ok := value.(*types.Document)
			if !ok {
				return State{}, false, fmt.Errorf("replication control state has type %T, want document", value)
			}
			state, err := decodeStateDocument(stateDocument)
			if err != nil {
				return State{}, false, err
			}
			return state, true, nil
		default:
			return State{}, false, fmt.Errorf("unsupported replication control format version %v", version)
		}
	}
}

func (s *collectionStorage) save(ctx context.Context, state State) error {
	stateDocument, err := encodeStateDocument(state)
	if err != nil {
		return err
	}
	document, err := types.NewDocument(
		"_id", controlDocumentID,
		"formatVersion", controlFormatVersion,
		"state", stateDocument,
	)
	if err != nil {
		return fmt.Errorf("constructing replication control document: %w", err)
	}
	updated, err := s.collection.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{document}})
	if err != nil {
		return fmt.Errorf("updating replication control document: %w", err)
	}
	if updated.Updated != 0 {
		return nil
	}
	if _, err := s.collection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{document}}); err != nil {
		return fmt.Errorf("inserting replication control document: %w", err)
	}
	return nil
}

func encodeStateDocument(state State) (*types.Document, error) {
	state = encodeStateMapKeys(state)
	var buffer bytes.Buffer
	valueWriter, err := bsonrw.NewBSONValueWriter(&buffer)
	if err != nil {
		return nil, fmt.Errorf("creating replication control BSON writer: %w", err)
	}
	encoder, err := mongobson.NewEncoder(valueWriter)
	if err != nil {
		return nil, fmt.Errorf("creating replication control BSON encoder: %w", err)
	}
	encoder.UseJSONStructTags()
	if err := encoder.Encode(state); err != nil {
		return nil, fmt.Errorf("encoding replication control state as BSON: %w", err)
	}
	document, err := dumbobson.ToDocument(wirebson.RawDocument(buffer.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("converting replication control BSON: %w", err)
	}
	return document, nil
}

func decodeStateDocument(document *types.Document) (State, error) {
	raw, err := dumbobson.FromDocumentRaw(document)
	if err != nil {
		return State{}, fmt.Errorf("encoding stored replication control document: %w", err)
	}
	decoder, err := mongobson.NewDecoder(bsonrw.NewBSONDocumentReader(raw))
	if err != nil {
		return State{}, fmt.Errorf("creating replication control BSON decoder: %w", err)
	}
	decoder.UseJSONStructTags()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decoding replication control BSON: %w", err)
	}
	state, err = decodeStateMapKeys(state)
	if err != nil {
		return State{}, err
	}
	return state, nil
}

func encodeStateMapKeys(state State) State {
	state = cloneState(state)
	state.CollectionMappings = encodeMapKeys(state.CollectionMappings)
	state.TransactionParts = encodeMapKeys(state.TransactionParts)
	state.AuthOwnership = encodeMapKeys(state.AuthOwnership)
	state.ReplicationMetadata = encodeMapKeys(state.ReplicationMetadata)
	if state.PendingPublication != nil {
		state.PendingPublication.Commits = encodeMapKeys(state.PendingPublication.Commits)
		if state.PendingPublication.PreApplyState != nil {
			preApply := state.PendingPublication.PreApplyState
			preApply.CollectionMappings = encodeMapKeys(preApply.CollectionMappings)
			preApply.TransactionParts = encodeMapKeys(preApply.TransactionParts)
			preApply.AuthOwnership = encodeMapKeys(preApply.AuthOwnership)
			preApply.ReplicationMetadata = encodeMapKeys(preApply.ReplicationMetadata)
		}
	}
	return state
}

func decodeStateMapKeys(state State) (State, error) {
	var err error
	if state.CollectionMappings, err = decodeMapKeys(state.CollectionMappings); err != nil {
		return State{}, fmt.Errorf("decoding collection mapping keys: %w", err)
	}
	if state.TransactionParts, err = decodeMapKeys(state.TransactionParts); err != nil {
		return State{}, fmt.Errorf("decoding transaction fragment keys: %w", err)
	}
	if state.AuthOwnership, err = decodeMapKeys(state.AuthOwnership); err != nil {
		return State{}, fmt.Errorf("decoding auth ownership keys: %w", err)
	}
	if state.ReplicationMetadata, err = decodeMapKeys(state.ReplicationMetadata); err != nil {
		return State{}, fmt.Errorf("decoding replication metadata keys: %w", err)
	}
	if state.PendingPublication != nil {
		if state.PendingPublication.Commits, err = decodeMapKeys(state.PendingPublication.Commits); err != nil {
			return State{}, fmt.Errorf("decoding pending publication commit keys: %w", err)
		}
		if state.PendingPublication.PreApplyState != nil {
			preApply := state.PendingPublication.PreApplyState
			if preApply.CollectionMappings, err = decodeMapKeys(preApply.CollectionMappings); err != nil {
				return State{}, fmt.Errorf("decoding pre-apply collection mapping keys: %w", err)
			}
			if preApply.TransactionParts, err = decodeMapKeys(preApply.TransactionParts); err != nil {
				return State{}, fmt.Errorf("decoding pre-apply transaction fragment keys: %w", err)
			}
			if preApply.AuthOwnership, err = decodeMapKeys(preApply.AuthOwnership); err != nil {
				return State{}, fmt.Errorf("decoding pre-apply auth ownership keys: %w", err)
			}
			if preApply.ReplicationMetadata, err = decodeMapKeys(preApply.ReplicationMetadata); err != nil {
				return State{}, fmt.Errorf("decoding pre-apply replication metadata keys: %w", err)
			}
		}
	}
	return state, nil
}

func encodeMapKeys[V any](values map[string]V) map[string]V {
	encoded := make(map[string]V, len(values))
	for key, value := range values {
		encoded[base64.RawURLEncoding.EncodeToString([]byte(key))] = value
	}
	return encoded
}

func decodeMapKeys[V any](values map[string]V) (map[string]V, error) {
	decoded := make(map[string]V, len(values))
	for key, value := range values {
		decodedKey, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil {
			return nil, fmt.Errorf("invalid encoded map key %q: %w", key, err)
		}
		decoded[string(decodedKey)] = value
	}
	return decoded, nil
}

func validateCommitIntervals(intervals []CommitInterval) error {
	commitIDs := make(map[string]struct{}, len(intervals))
	for index, interval := range intervals {
		if interval.CommitID == "" {
			return fmt.Errorf("commit interval %d has no commit ID", index)
		}
		if interval.First.Compare(interval.Last) > 0 {
			return fmt.Errorf("commit interval %d starts after it ends", index)
		}
		if _, ok := commitIDs[interval.CommitID]; ok {
			return fmt.Errorf("commit interval %d repeats commit ID %q", index, interval.CommitID)
		}
		commitIDs[interval.CommitID] = struct{}{}
		for commitIndex, commit := range interval.Commits {
			if commit.Database == "" || commit.CommitID == "" {
				return fmt.Errorf("commit interval %d has incomplete database commit %d", index, commitIndex)
			}
			if commitIndex > 0 && interval.Commits[commitIndex-1].Database >= commit.Database {
				return fmt.Errorf("commit interval %d database commits are not strictly sorted", index)
			}
		}
		if index > 0 && intervals[index-1].Last.Compare(interval.First) >= 0 {
			return fmt.Errorf("commit interval %d overlaps or precedes commit %q", index, intervals[index-1].CommitID)
		}
	}
	return nil
}

func validatePublishedCheckpoint(interval CommitInterval, checkpoint Checkpoint) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	if checkpoint.Written != interval.Last || checkpoint.Durable != interval.Last || checkpoint.Applied != interval.Last {
		return errors.New("checkpoint does not publish the complete commit interval")
	}
	return nil
}
