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

func (s *Store) BeginRollback(attempt RollbackAttempt) error {
	if attempt.ID == "" || attempt.Source == "" || attempt.SourceRBID < 0 || attempt.CommitID == "" ||
		attempt.OpTime == (OpTime{}) || attempt.AuditBranch == "" {
		return errors.New("rollback attempt requires identity, source, common point, and audit branch")
	}
	if !slices.IsSortedFunc(attempt.Databases, func(left, right RollbackDatabase) int {
		return compareStrings(left.Database, right.Database)
	}) {
		return errors.New("rollback databases are not sorted")
	}
	for index, database := range attempt.Databases {
		if database.Database == "" || database.CommitID == "" {
			return fmt.Errorf("rollback database %d is incomplete", index)
		}
		if index > 0 && attempt.Databases[index-1].Database == database.Database {
			return fmt.Errorf("rollback repeats database %q", database.Database)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.PendingPublication != nil {
		return errors.New("cannot begin rollback while a publication is pending")
	}
	if s.state.PendingRollback != nil {
		if rollbackAttemptsEqual(*s.state.PendingRollback, attempt) {
			return nil
		}
		return fmt.Errorf("rollback %q is already pending", s.state.PendingRollback.ID)
	}
	index, ok := s.commitByID[attempt.CommitID]
	if !ok || s.state.CommitIntervals[index].Last != attempt.OpTime {
		return errors.New("rollback common point is not retained")
	}
	copy := attempt
	copy.Databases = append([]RollbackDatabase(nil), attempt.Databases...)
	s.state.PendingRollback = &copy
	if err := s.persistLocked(); err != nil {
		s.state.PendingRollback = nil
		return err
	}
	return nil
}

func (s *Store) PendingRollback() (RollbackAttempt, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.PendingRollback == nil {
		return RollbackAttempt{}, false
	}
	copy := *s.state.PendingRollback
	copy.Databases = append([]RollbackDatabase(nil), copy.Databases...)
	return copy, true
}

func rollbackAttemptsEqual(left, right RollbackAttempt) bool {
	return left.ID == right.ID && left.Source == right.Source && left.SourceRBID == right.SourceRBID &&
		left.CommitID == right.CommitID && left.OpTime == right.OpTime && left.AuditBranch == right.AuditBranch &&
		slices.Equal(left.Databases, right.Databases)
}

func compareStrings(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
