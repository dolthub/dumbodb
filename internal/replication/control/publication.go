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
	"errors"
	"fmt"
	"slices"
)

var ErrPublicationComplete = errors.New("replication publication is already complete")

func (s *Store) BeginPublication(publication PendingPublication) error {
	if err := validatePendingPublication(publication); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if index, ok := s.commitByID[publication.ID]; ok {
		interval := s.state.CommitIntervals[index]
		if interval.First == publication.First && interval.Last == publication.Last {
			return ErrPublicationComplete
		}
		return fmt.Errorf("publication ID %q already identifies interval %v", publication.ID, interval)
	}
	if s.state.PendingPublication != nil {
		if samePublicationPlan(*s.state.PendingPublication, publication) {
			return nil
		}
		return fmt.Errorf("publication %q is already pending", s.state.PendingPublication.ID)
	}
	copy := clonePendingPublication(publication)
	s.state.PendingPublication = &copy
	return s.persistLocked()
}

func (s *Store) RecordPublicationCommit(publicationID, database, commitID string) error {
	if publicationID == "" || database == "" || commitID == "" {
		return errors.New("publication ID, database, and commit ID are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.state.PendingPublication
	if pending == nil || pending.ID != publicationID {
		return fmt.Errorf("publication %q is not pending", publicationID)
	}
	if _, ok := slices.BinarySearch(pending.Databases, database); !ok {
		return fmt.Errorf("database %q is not part of publication %q", database, publicationID)
	}
	if existing := pending.Commits[database]; existing != "" {
		if existing == commitID {
			return nil
		}
		return fmt.Errorf("database %q publication commit changed from %q to %q", database, existing, commitID)
	}
	pending.Commits[database] = commitID
	return s.persistLocked()
}

func (s *Store) PendingPublication() (PendingPublication, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.PendingPublication == nil {
		return PendingPublication{}, false
	}
	return clonePendingPublication(*s.state.PendingPublication), true
}

func validatePendingPublication(publication PendingPublication) error {
	if publication.ID == "" || publication.First == (OpTime{}) || publication.Last == (OpTime{}) {
		return errors.New("publication ID and source interval are required")
	}
	if publication.First.Compare(publication.Last) > 0 {
		return errors.New("publication starts after it ends")
	}
	if err := validatePublishedCheckpoint(CommitInterval{First: publication.First, Last: publication.Last, CommitID: publication.ID}, publication.Checkpoint); err != nil {
		return err
	}
	if !slices.IsSorted(publication.Databases) {
		return errors.New("publication databases are not sorted")
	}
	for index, database := range publication.Databases {
		if database == "" {
			return errors.New("publication contains an empty database")
		}
		if index > 0 && publication.Databases[index-1] == database {
			return fmt.Errorf("publication repeats database %q", database)
		}
	}
	if publication.Commits == nil {
		publication.Commits = make(map[string]string)
	}
	for database, commitID := range publication.Commits {
		if _, ok := slices.BinarySearch(publication.Databases, database); !ok || commitID == "" {
			return fmt.Errorf("publication has invalid commit for database %q", database)
		}
	}
	return nil
}

func clonePendingPublication(publication PendingPublication) PendingPublication {
	publication.Databases = append([]string(nil), publication.Databases...)
	commits := make(map[string]string, len(publication.Commits))
	for database, commitID := range publication.Commits {
		commits[database] = commitID
	}
	publication.Commits = commits
	return publication
}

func samePublicationPlan(left, right PendingPublication) bool {
	return left.ID == right.ID && left.First == right.First && left.Last == right.Last && left.Checkpoint == right.Checkpoint && slices.Equal(left.Databases, right.Databases)
}

func validatePendingCompletion(pending PendingPublication, interval CommitInterval, checkpoint Checkpoint) error {
	if pending.ID != interval.CommitID || pending.First != interval.First || pending.Last != interval.Last || pending.Checkpoint != checkpoint {
		return errors.New("completed publication does not match its pending record")
	}
	if len(interval.Commits) != len(pending.Databases) {
		return errors.New("completed publication does not contain every database commit")
	}
	for index, database := range pending.Databases {
		commit := interval.Commits[index]
		if commit.Database != database || commit.CommitID == "" || pending.Commits[database] != commit.CommitID {
			return fmt.Errorf("completed publication has no recorded commit for database %q", database)
		}
	}
	return nil
}
