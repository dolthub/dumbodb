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

// Package control persists replication state outside replicated user data.
package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
)

const stateFileName = "state.json"

type Lifecycle string

const (
	LifecycleActive   Lifecycle = "active"
	LifecycleDetached Lifecycle = "detached"
)

type InitialSyncPhase string

const (
	InitialSyncNotStarted InitialSyncPhase = "not_started"
	InitialSyncCloning    InitialSyncPhase = "cloning"
	InitialSyncApplying   InitialSyncPhase = "applying"
	InitialSyncComplete   InitialSyncPhase = "complete"
)

type Configuration struct {
	SetName    string `json:"set_name"`
	MemberHost string `json:"member_host"`
}

type Identity struct {
	ReplicaSetID  string `json:"replica_set_id"`
	MemberID      int    `json:"member_id"`
	ConfigVersion int64  `json:"config_version"`
	ConfigTerm    int64  `json:"config_term"`
	Term          int64  `json:"term"`
}

type MemberConfiguration struct {
	MemberID int     `json:"member_id"`
	Host     string  `json:"host"`
	Hidden   bool    `json:"hidden"`
	Priority float64 `json:"priority"`
	Votes    int     `json:"votes"`
}

type ReplicaConfiguration struct {
	SetName         string                `json:"set_name"`
	Version         int64                 `json:"version"`
	Term            int64                 `json:"term"`
	ProtocolVersion int64                 `json:"protocol_version"`
	ReplicaSetID    string                `json:"replica_set_id"`
	Members         []MemberConfiguration `json:"members"`
	RawBSON         []byte                `json:"raw_bson,omitempty"`
}

type OpTime struct {
	Seconds   uint32 `json:"seconds"`
	Increment uint32 `json:"increment"`
	Term      int64  `json:"term"`
}

func (o OpTime) Compare(other OpTime) int {
	if o.Term != other.Term {
		if o.Term < other.Term {
			return -1
		}
		return 1
	}
	if o.Seconds != other.Seconds {
		if o.Seconds < other.Seconds {
			return -1
		}
		return 1
	}
	if o.Increment < other.Increment {
		return -1
	}
	if o.Increment > other.Increment {
		return 1
	}
	return 0
}

type Checkpoint struct {
	Fetched  OpTime `json:"fetched"`
	Buffered OpTime `json:"buffered"`
	Written  OpTime `json:"written"`
	Durable  OpTime `json:"durable"`
	Applied  OpTime `json:"applied"`
}

type CommitInterval struct {
	First    OpTime           `json:"first"`
	Last     OpTime           `json:"last"`
	CommitID string           `json:"commit_id"`
	Commits  []DatabaseCommit `json:"commits,omitempty"`
}

type DatabaseCommit struct {
	Database string `json:"database"`
	CommitID string `json:"commit_id"`
}

type PendingPublication struct {
	ID         string            `json:"id"`
	First      OpTime            `json:"first"`
	Last       OpTime            `json:"last"`
	Databases  []string          `json:"databases"`
	Commits    map[string]string `json:"commits"`
	Checkpoint Checkpoint        `json:"checkpoint"`
	Ready      bool              `json:"ready"`
}

type CollectionMapping struct {
	SourceUUID       string `json:"source_uuid"`
	Database         string `json:"database"`
	Collection       string `json:"collection"`
	LocalUUID        string `json:"local_uuid"`
	CreateOpTime     OpTime `json:"create_optime"`
	LastUpdateOpTime OpTime `json:"last_update_optime"`
	Dropped          bool   `json:"dropped"`
	DropOpTime       OpTime `json:"drop_optime"`
}

type TransactionFragment struct {
	Key     string `json:"key"`
	First   OpTime `json:"first"`
	Last    OpTime `json:"last"`
	Payload []byte `json:"payload"`
}

type AuthOwnership struct {
	Namespace        string `json:"namespace"`
	Identity         string `json:"identity"`
	Owner            string `json:"owner"`
	LastUpdateOpTime OpTime `json:"last_update_optime"`
	Dropped          bool   `json:"dropped"`
}

type MetadataKind string

const (
	MetadataTransaction MetadataKind = "transaction"
	MetadataRetryImage  MetadataKind = "retry_image"
)

type ReplicationMetadataRecord struct {
	Kind             MetadataKind `json:"kind"`
	Namespace        string       `json:"namespace"`
	Key              string       `json:"key"`
	Document         []byte       `json:"document,omitempty"`
	LastUpdateOpTime OpTime       `json:"last_update_optime"`
	Deleted          bool         `json:"deleted"`
}

type InitialSyncAttempt struct {
	ID                  string `json:"id"`
	Source              string `json:"source"`
	SourceRBID          int64  `json:"source_rbid"`
	SourceInitialSyncID string `json:"source_initial_sync_id,omitempty"`
	BeginFetch          OpTime `json:"begin_fetch"`
	BeginApply          OpTime `json:"begin_apply"`
	Stop                OpTime `json:"stop"`
}

type State struct {
	Configuration       Configuration                        `json:"configuration"`
	Lifecycle           Lifecycle                            `json:"lifecycle"`
	Identity            *Identity                            `json:"identity,omitempty"`
	ReplicaConfig       *ReplicaConfiguration                `json:"replica_configuration,omitempty"`
	CurrentSource       string                               `json:"current_source"`
	CurrentRBID         int64                                `json:"current_rbid"`
	InitialSyncPhase    InitialSyncPhase                     `json:"initial_sync_phase"`
	InitialSyncAttempt  *InitialSyncAttempt                  `json:"initial_sync_attempt,omitempty"`
	Checkpoint          Checkpoint                           `json:"checkpoint"`
	CommitLogGeneration uint64                               `json:"commit_log_generation"`
	CommitIntervals     []CommitInterval                     `json:"commit_intervals,omitempty"`
	PendingPublication  *PendingPublication                  `json:"pending_publication,omitempty"`
	CollectionMappings  map[string]CollectionMapping         `json:"collection_mappings"`
	TransactionParts    map[string]TransactionFragment       `json:"transaction_parts"`
	AuthOwnership       map[string]AuthOwnership             `json:"auth_ownership"`
	ReplicationMetadata map[string]ReplicationMetadataRecord `json:"replication_metadata"`
}

type Store struct {
	mu         sync.RWMutex
	path       string
	state      State
	commitByID map[string]int
}

func Open(dir string, configuration Configuration) (*Store, error) {
	if err := validateConfiguration(configuration); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating replication control directory: %w", err)
	}

	store := &Store{path: filepath.Join(dir, stateFileName)}
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		store.state = newState(configuration)
		store.commitByID = make(map[string]int)
		if err := store.replaceCommitLogLocked(store.state.CommitLogGeneration, nil); err != nil {
			return nil, err
		}
		if err := store.persistLocked(); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading replication control state: %w", err)
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("decoding replication control state: %w", err)
	}
	legacyIntervals := append([]CommitInterval(nil), store.state.CommitIntervals...)
	normalizeState(&store.state)
	if store.state.Configuration != configuration {
		return nil, fmt.Errorf("replication control state belongs to set %q member %q, not set %q member %q",
			store.state.Configuration.SetName,
			store.state.Configuration.MemberHost,
			configuration.SetName,
			configuration.MemberHost)
	}
	if store.state.CommitLogGeneration == 0 {
		store.state.CommitLogGeneration = 1
		if err := validateCommitIntervals(legacyIntervals); err != nil {
			return nil, fmt.Errorf("validating legacy commit intervals: %w", err)
		}
		if err := store.replaceCommitLogLocked(store.state.CommitLogGeneration, legacyIntervals); err != nil {
			return nil, err
		}
		store.state.CommitIntervals = legacyIntervals
		store.rebuildCommitIndexLocked()
		if err := store.persistLocked(); err != nil {
			return nil, err
		}
		return store, nil
	}
	intervals, publishedCheckpoint, err := store.loadCommitLogLocked(store.state.CommitLogGeneration)
	if err != nil {
		return nil, err
	}
	store.state.CommitIntervals = intervals
	store.rebuildCommitIndexLocked()
	if publishedCheckpoint != nil && publishedCheckpoint.Applied.Compare(store.state.Checkpoint.Applied) > 0 {
		store.state.Checkpoint = *publishedCheckpoint
	}
	if store.state.PendingPublication != nil {
		if _, ok := store.commitByID[store.state.PendingPublication.ID]; ok {
			store.state.PendingPublication = nil
		}
	}
	if publishedCheckpoint != nil && publishedCheckpoint.Applied.Compare(store.state.Checkpoint.Applied) >= 0 {
		if err := store.persistLocked(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.state)
}

func (s *Store) InstallMember(identity Identity, member MemberConfiguration) error {
	if identity.ReplicaSetID == "" {
		return errors.New("replica set ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateMember(s.state.Configuration, identity, member); err != nil {
		return err
	}
	if s.state.Identity != nil {
		if s.state.Identity.ReplicaSetID != identity.ReplicaSetID {
			return fmt.Errorf("replica set ID changed from %q to %q", s.state.Identity.ReplicaSetID, identity.ReplicaSetID)
		}
		if s.state.Identity.MemberID != identity.MemberID {
			return fmt.Errorf("member ID changed from %d to %d", s.state.Identity.MemberID, identity.MemberID)
		}
		if compareConfiguration(identity, *s.state.Identity) < 0 {
			return fmt.Errorf("refusing stale replica configuration term %d version %d", identity.ConfigTerm, identity.ConfigVersion)
		}
		if identity.Term < s.state.Identity.Term {
			identity.Term = s.state.Identity.Term
		}
	}
	s.state.Identity = &identity
	return s.persistLocked()
}

func (s *Store) InstallReplicaConfiguration(configuration ReplicaConfiguration, currentTerm int64) error {
	if configuration.SetName == "" || configuration.ReplicaSetID == "" || len(configuration.Members) == 0 {
		return errors.New("replica configuration set name, replica set ID, and members are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if configuration.SetName != s.state.Configuration.SetName {
		return fmt.Errorf("replica configuration set %q does not match %q", configuration.SetName, s.state.Configuration.SetName)
	}
	var ownMember *MemberConfiguration
	for i := range configuration.Members {
		if configuration.Members[i].Host == s.state.Configuration.MemberHost {
			ownMember = &configuration.Members[i]
			break
		}
	}
	if ownMember == nil {
		return fmt.Errorf("replica configuration does not contain member %q", s.state.Configuration.MemberHost)
	}
	identity := Identity{
		ReplicaSetID:  configuration.ReplicaSetID,
		MemberID:      ownMember.MemberID,
		ConfigVersion: configuration.Version,
		ConfigTerm:    configuration.Term,
		Term:          currentTerm,
	}
	if err := validateMember(s.state.Configuration, identity, *ownMember); err != nil {
		return err
	}
	if s.state.Identity != nil {
		if s.state.Identity.ReplicaSetID != identity.ReplicaSetID {
			return fmt.Errorf("replica set ID changed from %q to %q", s.state.Identity.ReplicaSetID, identity.ReplicaSetID)
		}
		if s.state.Identity.MemberID != identity.MemberID {
			return fmt.Errorf("member ID changed from %d to %d", s.state.Identity.MemberID, identity.MemberID)
		}
		if compareConfiguration(identity, *s.state.Identity) < 0 {
			return fmt.Errorf("refusing stale replica configuration term %d version %d", identity.ConfigTerm, identity.ConfigVersion)
		}
		if identity.Term < s.state.Identity.Term {
			identity.Term = s.state.Identity.Term
		}
	}
	configuration.Members = append([]MemberConfiguration(nil), configuration.Members...)
	configuration.RawBSON = append([]byte(nil), configuration.RawBSON...)
	s.state.Identity = &identity
	s.state.ReplicaConfig = &configuration
	return s.persistLocked()
}

func (s *Store) ObserveTerm(term int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Identity == nil {
		return errors.New("cannot observe a term before installing member identity")
	}
	if term <= s.state.Identity.Term {
		return nil
	}
	s.state.Identity.Term = term
	return s.persistLocked()
}

func (s *Store) SetSource(host string, rbid int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Lifecycle != LifecycleActive {
		return errors.New("cannot set a sync source for a detached member")
	}
	s.state.CurrentSource = host
	s.state.CurrentRBID = rbid
	return s.persistLocked()
}

func (s *Store) SetInitialSyncPhase(phase InitialSyncPhase) error {
	if phase != InitialSyncNotStarted && phase != InitialSyncCloning && phase != InitialSyncApplying && phase != InitialSyncComplete {
		return fmt.Errorf("unknown initial sync phase %q", phase)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.InitialSyncPhase = phase
	return s.persistLocked()
}

func (s *Store) BeginInitialSync(attempt InitialSyncAttempt) error {
	if attempt.ID == "" || attempt.Source == "" || attempt.SourceRBID < 0 {
		return errors.New("initial sync attempt ID, source, and non-negative rollback ID are required")
	}
	if attempt.BeginFetch == (OpTime{}) || attempt.BeginApply == (OpTime{}) {
		return errors.New("initial sync begin-fetch and begin-apply positions are required")
	}
	if attempt.BeginApply.Compare(attempt.BeginFetch) < 0 {
		return errors.New("initial sync begin-apply position precedes begin-fetch position")
	}
	if attempt.Stop != (OpTime{}) {
		return errors.New("initial sync stop position must not be set when cloning begins")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := attempt
	s.state.InitialSyncAttempt = &copy
	s.state.InitialSyncPhase = InitialSyncCloning
	return s.persistLocked()
}

func (s *Store) SetInitialSyncStop(attemptID string, stop OpTime) error {
	if attemptID == "" || stop == (OpTime{}) {
		return errors.New("initial sync attempt ID and stop position are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.InitialSyncAttempt == nil || s.state.InitialSyncAttempt.ID != attemptID {
		return fmt.Errorf("initial sync attempt %q is not active", attemptID)
	}
	if stop.Compare(s.state.InitialSyncAttempt.BeginApply) < 0 {
		return errors.New("initial sync stop position precedes begin-apply position")
	}
	s.state.InitialSyncAttempt.Stop = stop
	s.state.InitialSyncPhase = InitialSyncApplying
	return s.persistLocked()
}

func (s *Store) CompleteInitialSync(attemptID string, checkpoint Checkpoint, finalRBID int64) error {
	if attemptID == "" || finalRBID < 0 {
		return errors.New("initial sync attempt ID and non-negative final rollback ID are required")
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt := s.state.InitialSyncAttempt
	if attempt == nil || attempt.ID != attemptID {
		return fmt.Errorf("initial sync attempt %q is not active", attemptID)
	}
	if attempt.Stop == (OpTime{}) {
		return errors.New("initial sync attempt has no stop position")
	}
	if finalRBID != attempt.SourceRBID {
		return fmt.Errorf("initial sync source rollback ID changed from %d to %d", attempt.SourceRBID, finalRBID)
	}
	if checkpoint.Applied != attempt.Stop || checkpoint.Durable != attempt.Stop || checkpoint.Written != attempt.Stop {
		return fmt.Errorf("initial sync checkpoint does not reach stop position %v", attempt.Stop)
	}
	s.state.Checkpoint = checkpoint
	s.state.CurrentSource = attempt.Source
	s.state.CurrentRBID = finalRBID
	s.state.InitialSyncPhase = InitialSyncComplete
	return s.persistLocked()
}

func (s *Store) ResetInitialSync(attemptID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.InitialSyncAttempt != nil && attemptID != "" && s.state.InitialSyncAttempt.ID != attemptID {
		return fmt.Errorf("initial sync attempt %q is not active", attemptID)
	}
	previousState := cloneState(s.state)
	newGeneration := s.state.CommitLogGeneration + 1
	if newGeneration == 0 {
		return errors.New("commit interval log generation exhausted")
	}
	if err := s.replaceCommitLogLocked(newGeneration, nil); err != nil {
		return err
	}
	s.state.InitialSyncAttempt = nil
	s.state.InitialSyncPhase = InitialSyncNotStarted
	s.state.Checkpoint = Checkpoint{}
	s.state.CurrentRBID = 0
	s.state.CommitLogGeneration = newGeneration
	s.state.CommitIntervals = nil
	s.commitByID = make(map[string]int)
	s.state.PendingPublication = nil
	s.state.CollectionMappings = make(map[string]CollectionMapping)
	s.state.TransactionParts = make(map[string]TransactionFragment)
	s.state.AuthOwnership = make(map[string]AuthOwnership)
	s.state.ReplicationMetadata = make(map[string]ReplicationMetadataRecord)
	if err := s.persistLocked(); err != nil {
		s.state = previousState
		return err
	}
	s.removeCommitLogLocked(previousState.CommitLogGeneration)
	return nil
}

func (s *Store) SetCheckpoint(checkpoint Checkpoint) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if checkpointRegresses(checkpoint, s.state.Checkpoint) {
		return errors.New("replication checkpoint moved backwards")
	}
	s.state.Checkpoint = checkpoint
	return s.persistLocked()
}

func validateCheckpoint(checkpoint Checkpoint) error {
	if checkpoint.Buffered.Compare(checkpoint.Fetched) > 0 || checkpoint.Written.Compare(checkpoint.Buffered) > 0 || checkpoint.Durable.Compare(checkpoint.Written) > 0 || checkpoint.Applied.Compare(checkpoint.Durable) > 0 {
		return errors.New("replication checkpoint positions are not ordered")
	}
	return nil
}

func (s *Store) ResetFetchProgress() (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Checkpoint.Fetched = s.state.Checkpoint.Written
	s.state.Checkpoint.Buffered = s.state.Checkpoint.Written
	if err := s.persistLocked(); err != nil {
		return Checkpoint{}, err
	}
	return s.state.Checkpoint, nil
}

func (s *Store) RecordCommit(interval CommitInterval) error {
	if interval.CommitID == "" {
		return errors.New("commit ID is required")
	}
	if interval.First.Compare(interval.Last) > 0 {
		return errors.New("commit interval starts after it ends")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.CommitIntervals) == 0 || interval.First.Compare(s.state.CommitIntervals[len(s.state.CommitIntervals)-1].Last) > 0 {
		if err := s.appendCommitIntervalLocked(interval, nil); err != nil {
			return err
		}
		s.state.CommitIntervals = append(s.state.CommitIntervals, interval)
		s.commitByID[interval.CommitID] = len(s.state.CommitIntervals) - 1
		return nil
	}
	index := sort.Search(len(s.state.CommitIntervals), func(i int) bool {
		return s.state.CommitIntervals[i].Last.Compare(interval.First) >= 0
	})
	if index < len(s.state.CommitIntervals) {
		existing := s.state.CommitIntervals[index]
		if equalCommitInterval(interval, existing) {
			return nil
		}
		if interval.Last.Compare(existing.First) >= 0 {
			return fmt.Errorf("commit interval %v overlaps existing interval for commit %q", interval, existing.CommitID)
		}
		return fmt.Errorf("commit interval %v precedes already recorded commit %q", interval, existing.CommitID)
	}
	return fmt.Errorf("commit interval %v is not after retained history", interval)
}

// PublishCommit durably records a commit interval and the checkpoint it makes reportable.
func (s *Store) PublishCommit(interval CommitInterval, checkpoint Checkpoint) error {
	if interval.CommitID == "" {
		return errors.New("commit ID is required")
	}
	if interval.First.Compare(interval.Last) > 0 {
		return errors.New("commit interval starts after it ends")
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	if checkpoint.Written != interval.Last || checkpoint.Durable != interval.Last || checkpoint.Applied != interval.Last {
		return errors.New("published checkpoint must write, durably store, and apply the complete commit interval")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if checkpointRegresses(checkpoint, s.state.Checkpoint) {
		return errors.New("replication checkpoint moved backwards")
	}
	if s.state.PendingPublication != nil {
		if err := validatePendingCompletion(*s.state.PendingPublication, interval, checkpoint); err != nil {
			return err
		}
	}
	if index, ok := s.commitByID[interval.CommitID]; ok {
		if !equalCommitInterval(s.state.CommitIntervals[index], interval) {
			return fmt.Errorf("commit ID %q already identifies interval %v", interval.CommitID, s.state.CommitIntervals[index])
		}
		if checkpoint.Applied.Compare(s.state.Checkpoint.Applied) <= 0 {
			return nil
		}
		return errors.New("published commit interval is missing its durable checkpoint record")
	}
	if len(s.state.CommitIntervals) != 0 {
		last := s.state.CommitIntervals[len(s.state.CommitIntervals)-1]
		if interval.First.Compare(last.Last) <= 0 {
			return fmt.Errorf("commit interval %v does not follow commit %q", interval, last.CommitID)
		}
	}
	if err := s.appendCommitIntervalLocked(interval, &checkpoint); err != nil {
		return err
	}
	s.state.CommitIntervals = append(s.state.CommitIntervals, interval)
	s.commitByID[interval.CommitID] = len(s.state.CommitIntervals) - 1
	s.state.Checkpoint = checkpoint
	if s.state.PendingPublication != nil && s.state.PendingPublication.ID == interval.CommitID {
		s.state.PendingPublication = nil
	}
	return s.persistLocked()
}

func (s *Store) CommitFor(opTime OpTime) (CommitInterval, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	index := sort.Search(len(s.state.CommitIntervals), func(i int) bool {
		return s.state.CommitIntervals[i].Last.Compare(opTime) >= 0
	})
	if index < len(s.state.CommitIntervals) {
		interval := s.state.CommitIntervals[index]
		if interval.First.Compare(opTime) <= 0 {
			return cloneCommitInterval(interval), true
		}
	}
	return CommitInterval{}, false
}

func (s *Store) CommitForID(commitID string) (CommitInterval, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	index, ok := s.commitByID[commitID]
	if !ok {
		return CommitInterval{}, false
	}
	return cloneCommitInterval(s.state.CommitIntervals[index]), true
}

func (s *Store) CommitForDatabaseCommit(database, commitID string) (CommitInterval, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, interval := range s.state.CommitIntervals {
		index, ok := slices.BinarySearchFunc(interval.Commits, DatabaseCommit{Database: database}, func(left, right DatabaseCommit) int {
			return strings.Compare(left.Database, right.Database)
		})
		if ok && interval.Commits[index].CommitID == commitID {
			return cloneCommitInterval(interval), true
		}
	}
	return CommitInterval{}, false
}

func (s *Store) PutCollectionMapping(mapping CollectionMapping) error {
	if mapping.SourceUUID == "" || mapping.Database == "" || mapping.Collection == "" || mapping.LocalUUID == "" {
		return errors.New("source UUID, database, collection, and local UUID are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if mapping.LastUpdateOpTime == (OpTime{}) {
		mapping.LastUpdateOpTime = mapping.CreateOpTime
	}
	if existing, ok := s.state.CollectionMappings[mapping.SourceUUID]; ok {
		if existing == mapping {
			return nil
		}
		return fmt.Errorf("collection mapping for source UUID %q already exists", mapping.SourceUUID)
	}
	for _, existing := range s.state.CollectionMappings {
		if !existing.Dropped && existing.Database == mapping.Database && existing.Collection == mapping.Collection {
			return fmt.Errorf("collection %q.%q is already mapped to source UUID %q", mapping.Database, mapping.Collection, existing.SourceUUID)
		}
	}
	s.state.CollectionMappings[mapping.SourceUUID] = mapping
	return s.persistLocked()
}

func (s *Store) RenameCollectionMapping(sourceUUID, oldDatabase, oldCollection, newDatabase, newCollection string, opTime OpTime) error {
	if sourceUUID == "" || oldDatabase == "" || oldCollection == "" || newDatabase == "" || newCollection == "" {
		return errors.New("source UUID and old/new namespaces are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mapping, ok := s.state.CollectionMappings[sourceUUID]
	if !ok {
		return fmt.Errorf("source UUID %q has no collection mapping", sourceUUID)
	}
	if mapping.Dropped {
		return fmt.Errorf("source UUID %q was dropped at %v", sourceUUID, mapping.DropOpTime)
	}
	if mapping.Database == newDatabase && mapping.Collection == newCollection && mapping.LastUpdateOpTime == opTime {
		return nil
	}
	if mapping.Database != oldDatabase || mapping.Collection != oldCollection {
		return fmt.Errorf("source UUID %q maps to %q.%q, not %q.%q", sourceUUID, mapping.Database, mapping.Collection, oldDatabase, oldCollection)
	}
	if opTime.Compare(mapping.LastUpdateOpTime) <= 0 {
		return fmt.Errorf("rename optime %v is not after mapping optime %v", opTime, mapping.LastUpdateOpTime)
	}
	for otherUUID, existing := range s.state.CollectionMappings {
		if otherUUID != sourceUUID && !existing.Dropped && existing.Database == newDatabase && existing.Collection == newCollection {
			return fmt.Errorf("rename target %q.%q is mapped to source UUID %q", newDatabase, newCollection, otherUUID)
		}
	}
	mapping.Database = newDatabase
	mapping.Collection = newCollection
	mapping.LastUpdateOpTime = opTime
	s.state.CollectionMappings[sourceUUID] = mapping
	return s.persistLocked()
}

func (s *Store) DropCollectionMapping(sourceUUID string, opTime OpTime) error {
	if sourceUUID == "" {
		return errors.New("source UUID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mapping, ok := s.state.CollectionMappings[sourceUUID]
	if !ok {
		return fmt.Errorf("source UUID %q has no collection mapping", sourceUUID)
	}
	if mapping.Dropped {
		if mapping.DropOpTime == opTime {
			return nil
		}
		return fmt.Errorf("source UUID %q was already dropped at %v", sourceUUID, mapping.DropOpTime)
	}
	if opTime.Compare(mapping.LastUpdateOpTime) <= 0 {
		return fmt.Errorf("drop optime %v is not after mapping optime %v", opTime, mapping.LastUpdateOpTime)
	}
	mapping.Dropped = true
	mapping.DropOpTime = opTime
	mapping.LastUpdateOpTime = opTime
	s.state.CollectionMappings[sourceUUID] = mapping
	return s.persistLocked()
}

func (s *Store) CollectionMapping(sourceUUID string) (CollectionMapping, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mapping, ok := s.state.CollectionMappings[sourceUUID]
	return mapping, ok
}

func (s *Store) ActiveCollectionMapping(sourceUUID string) (CollectionMapping, bool) {
	mapping, ok := s.CollectionMapping(sourceUUID)
	return mapping, ok && !mapping.Dropped
}

func (s *Store) PutTransactionFragment(fragment TransactionFragment) error {
	if fragment.Key == "" {
		return errors.New("transaction fragment key is required")
	}
	if fragment.First.Compare(fragment.Last) > 0 {
		return errors.New("transaction fragment starts after it ends")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.TransactionParts[fragment.Key] = fragment
	return s.persistLocked()
}

func (s *Store) DeleteTransactionFragment(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state.TransactionParts, key)
	return s.persistLocked()
}

func (s *Store) Detach() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Lifecycle = LifecycleDetached
	s.state.CurrentSource = ""
	return s.persistLocked()
}

func (s *Store) Activate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Lifecycle = LifecycleActive
	return s.persistLocked()
}

func validateConfiguration(configuration Configuration) error {
	if configuration.SetName == "" || configuration.MemberHost == "" {
		return errors.New("replica set name and member host are required")
	}
	return nil
}

func validateMember(configuration Configuration, identity Identity, member MemberConfiguration) error {
	if identity.MemberID != member.MemberID {
		return fmt.Errorf("member identity %d does not match configuration member %d", identity.MemberID, member.MemberID)
	}
	if configuration.MemberHost != member.Host {
		return fmt.Errorf("member host %q does not match configured host %q", member.Host, configuration.MemberHost)
	}
	if !member.Hidden || member.Priority != 0 || member.Votes != 0 {
		return fmt.Errorf("replication member %q must be hidden=true, priority=0, votes=0", member.Host)
	}
	return nil
}

func compareConfiguration(left, right Identity) int {
	if left.ConfigTerm != right.ConfigTerm {
		if left.ConfigTerm < right.ConfigTerm {
			return -1
		}
		return 1
	}
	if left.ConfigVersion < right.ConfigVersion {
		return -1
	}
	if left.ConfigVersion > right.ConfigVersion {
		return 1
	}
	return 0
}

func newState(configuration Configuration) State {
	return State{
		Configuration:       configuration,
		Lifecycle:           LifecycleActive,
		InitialSyncPhase:    InitialSyncNotStarted,
		CommitLogGeneration: 1,
		CollectionMappings:  make(map[string]CollectionMapping),
		TransactionParts:    make(map[string]TransactionFragment),
		AuthOwnership:       make(map[string]AuthOwnership),
		ReplicationMetadata: make(map[string]ReplicationMetadataRecord),
	}
}

func normalizeState(state *State) {
	if state.Lifecycle == "" {
		state.Lifecycle = LifecycleActive
	}
	if state.InitialSyncPhase == "" {
		state.InitialSyncPhase = InitialSyncNotStarted
	}
	if state.CollectionMappings == nil {
		state.CollectionMappings = make(map[string]CollectionMapping)
	}
	if state.TransactionParts == nil {
		state.TransactionParts = make(map[string]TransactionFragment)
	}
	if state.AuthOwnership == nil {
		state.AuthOwnership = make(map[string]AuthOwnership)
	}
	if state.ReplicationMetadata == nil {
		state.ReplicationMetadata = make(map[string]ReplicationMetadataRecord)
	}
	if state.PendingPublication != nil && state.PendingPublication.Commits == nil {
		state.PendingPublication.Commits = make(map[string]string)
	}
}

func (s *Store) rebuildCommitIndexLocked() {
	s.commitByID = make(map[string]int, len(s.state.CommitIntervals))
	for index, interval := range s.state.CommitIntervals {
		s.commitByID[interval.CommitID] = index
	}
}

func checkpointRegresses(next, current Checkpoint) bool {
	return next.Fetched.Compare(current.Fetched) < 0 ||
		next.Buffered.Compare(current.Buffered) < 0 ||
		next.Written.Compare(current.Written) < 0 ||
		next.Durable.Compare(current.Durable) < 0 ||
		next.Applied.Compare(current.Applied) < 0
}

func equalCommitInterval(left, right CommitInterval) bool {
	return left.First == right.First && left.Last == right.Last && left.CommitID == right.CommitID && slices.Equal(left.Commits, right.Commits)
}

func cloneCommitInterval(interval CommitInterval) CommitInterval {
	interval.Commits = append([]DatabaseCommit(nil), interval.Commits...)
	return interval
}

func cloneState(state State) State {
	data, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	var clone State
	if err := json.Unmarshal(data, &clone); err != nil {
		panic(err)
	}
	normalizeState(&clone)
	return clone
}

func (s *Store) persistLocked() error {
	persistedState := s.state
	persistedState.CommitIntervals = nil
	data, err := json.Marshal(persistedState)
	if err != nil {
		return fmt.Errorf("encoding replication control state: %w", err)
	}
	dir := filepath.Dir(s.path)
	temporary, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("creating replication control state: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("writing replication control state: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("securing replication control state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("syncing replication control state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("closing replication control state: %w", err)
	}
	if err := os.Rename(temporaryName, s.path); err != nil {
		return fmt.Errorf("publishing replication control state: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening replication control directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("syncing replication control directory: %w", err)
	}
	return nil
}
