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
	"sort"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dumbodb/internal/backends"
	dumbobson "github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	mongobson "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsonrw"
)

const (
	controlDocumentID              = "control"
	legacyBinaryControlVersion     = int32(1)
	legacyStructuredControlVersion = int32(2)
	controlFormatVersion           = int32(3)
	commitIntervalDocumentKind     = "commit_interval"
	commitIntervalDocumentPrefix   = "interval:"
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

func (s *collectionStorage) load(ctx context.Context) (State, int32, bool, error) {
	filter, err := types.NewDocument("_id", controlDocumentID)
	if err != nil {
		return State{}, 0, false, err
	}
	result, err := s.collection.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return State{}, 0, false, fmt.Errorf("querying replication control state: %w", err)
	}
	defer result.Iter.Close()
	for {
		_, document, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return State{}, 0, false, nil
		}
		if err != nil {
			return State{}, 0, false, fmt.Errorf("reading replication control state: %w", err)
		}
		id, _ := document.Get("_id")
		if id != controlDocumentID {
			continue
		}
		version, _ := document.Get("formatVersion")
		value, err := document.Get("state")
		if err != nil {
			return State{}, 0, false, errors.New("replication control document has no state")
		}
		switch version {
		case legacyBinaryControlVersion:
			binary, ok := value.(types.Binary)
			if !ok || binary.Subtype != types.BinaryGeneric {
				return State{}, 0, false, errors.New("legacy replication control state is not generic binary data")
			}
			var state State
			if err := json.Unmarshal(binary.B, &state); err != nil {
				return State{}, 0, false, fmt.Errorf("decoding legacy replication control state: %w", err)
			}
			return state, legacyBinaryControlVersion, true, nil
		case legacyStructuredControlVersion, controlFormatVersion:
			stateDocument, ok := value.(*types.Document)
			if !ok {
				return State{}, 0, false, fmt.Errorf("replication control state has type %T, want document", value)
			}
			state, err := decodeStateDocument(stateDocument)
			if err != nil {
				return State{}, 0, false, err
			}
			return state, version.(int32), true, nil
		default:
			return State{}, 0, false, fmt.Errorf("unsupported replication control format version %v", version)
		}
	}
}

func (s *collectionStorage) save(ctx context.Context, state State) error {
	state.CommitIntervals = nil
	stateDocument, err := encodeStateDocument(state)
	if err != nil {
		return err
	}
	document, err := types.NewDocument(
		"_id", controlDocumentID,
		"kind", "control",
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

type storedCommitInterval struct {
	Generation int64          `json:"generation"`
	Interval   CommitInterval `json:"interval"`
}

func (s *collectionStorage) loadCommitIntervals(ctx context.Context, generation uint64) ([]CommitInterval, error) {
	bsonGeneration, err := commitIntervalGeneration(generation)
	if err != nil {
		return nil, err
	}
	filter, err := types.NewDocument("kind", commitIntervalDocumentKind, "generation", bsonGeneration)
	if err != nil {
		return nil, err
	}
	result, err := s.collection.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return nil, fmt.Errorf("querying replication commit intervals: %w", err)
	}
	defer result.Iter.Close()
	var intervals []CommitInterval
	for {
		_, document, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading replication commit intervals: %w", err)
		}
		stored, err := decodeCommitIntervalDocument(document)
		if err != nil {
			return nil, err
		}
		if stored.Generation != bsonGeneration {
			return nil, errors.New("replication commit interval payload has a mismatched generation")
		}
		intervals = append(intervals, stored.Interval)
	}
	sort.Slice(intervals, func(left, right int) bool {
		return intervals[left].First.Compare(intervals[right].First) < 0
	})
	return intervals, nil
}

func (s *collectionStorage) appendCommitInterval(ctx context.Context, generation uint64, interval CommitInterval) error {
	document, err := encodeCommitIntervalDocument(generation, interval)
	if err != nil {
		return err
	}
	if _, err := s.collection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{document}}); err == nil {
		return nil
	} else if !backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
		return fmt.Errorf("inserting replication commit interval: %w", err)
	}
	id, _ := document.Get("_id")
	existing, found, err := s.findDocument(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("duplicate replication commit interval document %q was not found", id)
	}
	stored, err := decodeCommitIntervalDocument(existing)
	if err != nil {
		return err
	}
	if stored.Generation == int64(generation) && equalCommitInterval(stored.Interval, interval) {
		return nil
	}
	return fmt.Errorf("replication commit interval document %q disagrees with retained state", id)
}

func (s *collectionStorage) appendCommitIntervals(ctx context.Context, generation uint64, intervals []CommitInterval) error {
	for _, interval := range intervals {
		if err := s.appendCommitInterval(ctx, generation, interval); err != nil {
			return err
		}
	}
	return nil
}

func (s *collectionStorage) deleteCommitIntervals(ctx context.Context, generation uint64) error {
	bsonGeneration, err := commitIntervalGeneration(generation)
	if err != nil {
		return err
	}
	filter, err := types.NewDocument("kind", commitIntervalDocumentKind, "generation", bsonGeneration)
	if err != nil {
		return err
	}
	result, err := s.collection.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return fmt.Errorf("querying obsolete replication commit intervals: %w", err)
	}
	defer result.Iter.Close()
	var ids []any
	for {
		_, document, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading obsolete replication commit intervals: %w", err)
		}
		id, _ := document.Get("_id")
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.collection.DeleteAll(ctx, &backends.DeleteAllParams{IDs: ids}); err != nil {
		return fmt.Errorf("deleting obsolete replication commit intervals: %w", err)
	}
	return nil
}

func (s *collectionStorage) findDocument(ctx context.Context, id any) (*types.Document, bool, error) {
	filter, err := types.NewDocument("_id", id)
	if err != nil {
		return nil, false, err
	}
	result, err := s.collection.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return nil, false, fmt.Errorf("querying replication control document %v: %w", id, err)
	}
	defer result.Iter.Close()
	for {
		_, document, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("reading replication control document %v: %w", id, err)
		}
		documentID, _ := document.Get("_id")
		if documentID == id {
			return document, true, nil
		}
	}
}

func encodeCommitIntervalDocument(generation uint64, interval CommitInterval) (*types.Document, error) {
	bsonGeneration, err := commitIntervalGeneration(generation)
	if err != nil {
		return nil, err
	}
	stored := storedCommitInterval{Generation: bsonGeneration, Interval: cloneCommitInterval(interval)}
	payload, err := encodeBSONDocument(stored)
	if err != nil {
		return nil, fmt.Errorf("encoding replication commit interval: %w", err)
	}
	id := commitIntervalDocumentPrefix + fmt.Sprintf("%020d:", generation) +
		base64.RawURLEncoding.EncodeToString([]byte(interval.CommitID))
	return types.NewDocument(
		"_id", id,
		"kind", commitIntervalDocumentKind,
		"generation", bsonGeneration,
		"value", payload,
	)
}

func commitIntervalGeneration(generation uint64) (int64, error) {
	if generation > uint64(^uint64(0)>>1) {
		return 0, errors.New("replication commit interval generation exceeds BSON int64")
	}
	return int64(generation), nil
}

func decodeCommitIntervalDocument(document *types.Document) (storedCommitInterval, error) {
	value, err := document.Get("value")
	if err != nil {
		return storedCommitInterval{}, errors.New("replication commit interval document has no value")
	}
	payload, ok := value.(*types.Document)
	if !ok {
		return storedCommitInterval{}, fmt.Errorf("replication commit interval value has type %T, want document", value)
	}
	var stored storedCommitInterval
	if err := decodeBSONDocument(payload, &stored); err != nil {
		return storedCommitInterval{}, fmt.Errorf("decoding replication commit interval: %w", err)
	}
	return stored, nil
}

func encodeStateDocument(state State) (*types.Document, error) {
	state = encodeStateMapKeys(state)
	return encodeBSONDocument(state)
}

func encodeBSONDocument(value any) (*types.Document, error) {
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
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encoding structured BSON: %w", err)
	}
	document, err := dumbobson.ToDocument(wirebson.RawDocument(buffer.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("converting replication control BSON: %w", err)
	}
	return document, nil
}

func decodeStateDocument(document *types.Document) (State, error) {
	var state State
	if err := decodeBSONDocument(document, &state); err != nil {
		return State{}, fmt.Errorf("decoding replication control BSON: %w", err)
	}
	state, err := decodeStateMapKeys(state)
	if err != nil {
		return State{}, err
	}
	return state, nil
}

func decodeBSONDocument(document *types.Document, value any) error {
	raw, err := dumbobson.FromDocumentRaw(document)
	if err != nil {
		return fmt.Errorf("encoding stored structured document: %w", err)
	}
	decoder, err := mongobson.NewDecoder(bsonrw.NewBSONDocumentReader(raw))
	if err != nil {
		return fmt.Errorf("creating structured BSON decoder: %w", err)
	}
	decoder.UseJSONStructTags()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decoding structured BSON: %w", err)
	}
	return nil
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
