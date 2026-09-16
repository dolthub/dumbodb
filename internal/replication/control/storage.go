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
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

const (
	controlDocumentID    = "control"
	controlFormatVersion = int32(1)
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

func (s *collectionStorage) load(ctx context.Context) ([]byte, bool, error) {
	filter, err := types.NewDocument("_id", controlDocumentID)
	if err != nil {
		return nil, false, err
	}
	result, err := s.collection.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return nil, false, fmt.Errorf("querying replication control state: %w", err)
	}
	defer result.Iter.Close()
	for {
		_, document, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("reading replication control state: %w", err)
		}
		id, _ := document.Get("_id")
		if id != controlDocumentID {
			continue
		}
		version, _ := document.Get("formatVersion")
		if version != controlFormatVersion {
			return nil, false, fmt.Errorf("unsupported replication control format version %v", version)
		}
		value, err := document.Get("state")
		if err != nil {
			return nil, false, errors.New("replication control document has no state")
		}
		binary, ok := value.(types.Binary)
		if !ok || binary.Subtype != types.BinaryGeneric {
			return nil, false, errors.New("replication control document state is not generic binary data")
		}
		return append([]byte(nil), binary.B...), true, nil
	}
}

func (s *collectionStorage) save(ctx context.Context, state State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encoding replication control state: %w", err)
	}
	document, err := types.NewDocument(
		"_id", controlDocumentID,
		"formatVersion", controlFormatVersion,
		"state", types.Binary{B: data, Subtype: types.BinaryGeneric},
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
