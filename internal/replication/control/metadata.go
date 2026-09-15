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
)

func (s *Store) PutAuthOwnership(record AuthOwnership) error {
	if record.Namespace == "" || record.Identity == "" || record.Owner == "" || record.LastUpdateOpTime == (OpTime{}) {
		return errors.New("auth ownership requires namespace, identity, owner, and optime")
	}
	if !isAuthNamespace(record.Namespace) {
		return fmt.Errorf("unsupported auth ownership namespace %q", record.Namespace)
	}
	key := authOwnershipKey(record.Namespace, record.Identity)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.state.AuthOwnership[key]; ok {
		if existing.Owner != record.Owner {
			return fmt.Errorf("replicated identity %q is owned by %q, not %q", record.Identity, existing.Owner, record.Owner)
		}
		comparison := record.LastUpdateOpTime.Compare(existing.LastUpdateOpTime)
		if comparison < 0 {
			return fmt.Errorf("auth ownership optime %v precedes %v", record.LastUpdateOpTime, existing.LastUpdateOpTime)
		}
		if comparison == 0 {
			if existing == record {
				return nil
			}
			return fmt.Errorf("auth ownership at %v disagrees with existing state", record.LastUpdateOpTime)
		}
	}
	record.Dropped = false
	s.state.AuthOwnership[key] = record
	return s.persistLocked()
}

func (s *Store) DropAuthOwnership(namespace, identity, owner string, opTime OpTime) error {
	if namespace == "" || identity == "" || owner == "" || opTime == (OpTime{}) {
		return errors.New("dropping auth ownership requires namespace, identity, owner, and optime")
	}
	if !isAuthNamespace(namespace) {
		return fmt.Errorf("unsupported auth ownership namespace %q", namespace)
	}
	key := authOwnershipKey(namespace, identity)
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.AuthOwnership[key]
	if !ok {
		s.state.AuthOwnership[key] = AuthOwnership{
			Namespace: namespace, Identity: identity, Owner: owner, LastUpdateOpTime: opTime, Dropped: true,
		}
		return s.persistLocked()
	}
	if record.Owner != owner {
		return fmt.Errorf("replicated identity %q is owned by %q, not %q", identity, record.Owner, owner)
	}
	comparison := opTime.Compare(record.LastUpdateOpTime)
	if comparison < 0 {
		return fmt.Errorf("auth deletion optime %v precedes %v", opTime, record.LastUpdateOpTime)
	}
	if comparison == 0 {
		if record.Dropped {
			return nil
		}
		return fmt.Errorf("auth deletion at %v disagrees with existing state", opTime)
	}
	record.LastUpdateOpTime = opTime
	record.Dropped = true
	s.state.AuthOwnership[key] = record
	return s.persistLocked()
}

func (s *Store) AuthOwnershipFor(namespace, identity string) (AuthOwnership, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.state.AuthOwnership[authOwnershipKey(namespace, identity)]
	return record, ok
}

func (s *Store) PutReplicationMetadata(record ReplicationMetadataRecord) error {
	if err := validateMetadataRecord(record); err != nil {
		return err
	}
	key := metadataRecordKey(record.Kind, record.Key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.state.ReplicationMetadata[key]; ok {
		comparison := record.LastUpdateOpTime.Compare(existing.LastUpdateOpTime)
		if comparison < 0 {
			return fmt.Errorf("metadata optime %v precedes %v", record.LastUpdateOpTime, existing.LastUpdateOpTime)
		}
		if comparison == 0 {
			if metadataRecordsEqual(existing, record) {
				return nil
			}
			return fmt.Errorf("metadata at %v disagrees with existing state", record.LastUpdateOpTime)
		}
	}
	record.Document = append([]byte(nil), record.Document...)
	record.Deleted = false
	s.state.ReplicationMetadata[key] = record
	return s.persistLocked()
}

func (s *Store) DeleteReplicationMetadata(kind MetadataKind, key string, opTime OpTime) error {
	if key == "" || opTime == (OpTime{}) {
		return errors.New("deleting replication metadata requires key and optime")
	}
	namespace, err := metadataNamespace(kind)
	if err != nil {
		return err
	}
	mapKey := metadataRecordKey(kind, key)
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.ReplicationMetadata[mapKey]
	if !ok {
		s.state.ReplicationMetadata[mapKey] = ReplicationMetadataRecord{
			Kind: kind, Namespace: namespace, Key: key, LastUpdateOpTime: opTime, Deleted: true,
		}
		return s.persistLocked()
	}
	comparison := opTime.Compare(record.LastUpdateOpTime)
	if comparison < 0 {
		return fmt.Errorf("metadata deletion optime %v precedes %v", opTime, record.LastUpdateOpTime)
	}
	if comparison == 0 {
		if record.Deleted {
			return nil
		}
		return fmt.Errorf("metadata deletion at %v disagrees with existing state", opTime)
	}
	record.Document = nil
	record.LastUpdateOpTime = opTime
	record.Deleted = true
	s.state.ReplicationMetadata[mapKey] = record
	return s.persistLocked()
}

func (s *Store) ReplicationMetadataRecordFor(kind MetadataKind, key string) (ReplicationMetadataRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.state.ReplicationMetadata[metadataRecordKey(kind, key)]
	record.Document = append([]byte(nil), record.Document...)
	return record, ok
}

func validateMetadataRecord(record ReplicationMetadataRecord) error {
	expectedNamespace, err := metadataNamespace(record.Kind)
	if err != nil {
		return err
	}
	if record.Namespace != expectedNamespace {
		return fmt.Errorf("metadata kind %q requires namespace %q, not %q", record.Kind, expectedNamespace, record.Namespace)
	}
	if record.Namespace == "" || record.Key == "" || len(record.Document) == 0 || record.LastUpdateOpTime == (OpTime{}) {
		return errors.New("replication metadata requires namespace, key, document, and optime")
	}
	if record.Deleted {
		return errors.New("put replication metadata cannot create a deleted record")
	}
	return nil
}

func metadataNamespace(kind MetadataKind) (string, error) {
	switch kind {
	case MetadataTransaction:
		return "config.transactions", nil
	case MetadataRetryImage:
		return "config.image_collection", nil
	default:
		return "", fmt.Errorf("unknown replication metadata kind %q", kind)
	}
}

func authOwnershipKey(namespace, identity string) string {
	return namespace + "\x00" + identity
}

func isAuthNamespace(namespace string) bool {
	return namespace == "admin.system.users" || namespace == "admin.system.roles"
}

func metadataRecordKey(kind MetadataKind, key string) string {
	return string(kind) + "\x00" + key
}

func metadataRecordsEqual(left, right ReplicationMetadataRecord) bool {
	if left.Kind != right.Kind || left.Namespace != right.Namespace || left.Key != right.Key ||
		left.LastUpdateOpTime != right.LastUpdateOpTime || left.Deleted != right.Deleted || len(left.Document) != len(right.Document) {
		return false
	}
	for index := range left.Document {
		if left.Document[index] != right.Document[index] {
			return false
		}
	}
	return true
}
