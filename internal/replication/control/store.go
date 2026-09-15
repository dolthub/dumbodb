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
	"sort"
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
	Branch     string `json:"branch"`
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
	First    OpTime `json:"first"`
	Last     OpTime `json:"last"`
	CommitID string `json:"commit_id"`
}

type CollectionMapping struct {
	SourceUUID   string `json:"source_uuid"`
	Database     string `json:"database"`
	Collection   string `json:"collection"`
	LocalUUID    string `json:"local_uuid"`
	CreateOpTime OpTime `json:"create_optime"`
	Dropped      bool   `json:"dropped"`
	DropOpTime   OpTime `json:"drop_optime"`
}

type TransactionFragment struct {
	Key     string `json:"key"`
	First   OpTime `json:"first"`
	Last    OpTime `json:"last"`
	Payload []byte `json:"payload"`
}

type State struct {
	Configuration      Configuration                  `json:"configuration"`
	Lifecycle          Lifecycle                      `json:"lifecycle"`
	Identity           *Identity                      `json:"identity,omitempty"`
	CurrentSource      string                         `json:"current_source"`
	CurrentRBID        int64                          `json:"current_rbid"`
	InitialSyncPhase   InitialSyncPhase               `json:"initial_sync_phase"`
	Checkpoint         Checkpoint                     `json:"checkpoint"`
	CommitIntervals    []CommitInterval               `json:"commit_intervals"`
	CollectionMappings map[string]CollectionMapping   `json:"collection_mappings"`
	TransactionParts   map[string]TransactionFragment `json:"transaction_parts"`
}

type Store struct {
	mu    sync.RWMutex
	path  string
	state State
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
	normalizeState(&store.state)
	if store.state.Configuration != configuration {
		return nil, fmt.Errorf("replication control state belongs to set %q branch %q member %q, not set %q branch %q member %q",
			store.state.Configuration.SetName,
			store.state.Configuration.Branch,
			store.state.Configuration.MemberHost,
			configuration.SetName,
			configuration.Branch,
			configuration.MemberHost)
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

func (s *Store) SetCheckpoint(checkpoint Checkpoint) error {
	if checkpoint.Buffered.Compare(checkpoint.Fetched) > 0 || checkpoint.Written.Compare(checkpoint.Buffered) > 0 || checkpoint.Durable.Compare(checkpoint.Written) > 0 || checkpoint.Applied.Compare(checkpoint.Durable) > 0 {
		return errors.New("replication checkpoint positions are not ordered")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if checkpoint.Fetched.Compare(s.state.Checkpoint.Fetched) < 0 || checkpoint.Applied.Compare(s.state.Checkpoint.Applied) < 0 {
		return errors.New("replication checkpoint moved backwards")
	}
	s.state.Checkpoint = checkpoint
	return s.persistLocked()
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
	for _, existing := range s.state.CommitIntervals {
		if interval.First.Compare(existing.Last) <= 0 && interval.Last.Compare(existing.First) >= 0 {
			if interval == existing {
				return nil
			}
			return fmt.Errorf("commit interval %v overlaps existing interval for commit %q", interval, existing.CommitID)
		}
	}
	s.state.CommitIntervals = append(s.state.CommitIntervals, interval)
	sort.Slice(s.state.CommitIntervals, func(i, j int) bool {
		return s.state.CommitIntervals[i].First.Compare(s.state.CommitIntervals[j].First) < 0
	})
	return s.persistLocked()
}

func (s *Store) CommitFor(opTime OpTime) (CommitInterval, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, interval := range s.state.CommitIntervals {
		if interval.First.Compare(opTime) <= 0 && interval.Last.Compare(opTime) >= 0 {
			return interval, true
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
	if existing, ok := s.state.CollectionMappings[mapping.SourceUUID]; ok && existing.CreateOpTime.Compare(mapping.CreateOpTime) != 0 {
		return fmt.Errorf("collection mapping for source UUID %q changed creation optime", mapping.SourceUUID)
	}
	s.state.CollectionMappings[mapping.SourceUUID] = mapping
	return s.persistLocked()
}

func (s *Store) CollectionMapping(sourceUUID string) (CollectionMapping, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mapping, ok := s.state.CollectionMappings[sourceUUID]
	return mapping, ok
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
	if configuration.SetName == "" || configuration.Branch == "" || configuration.MemberHost == "" {
		return errors.New("replica set name, replication branch, and member host are required")
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
		Configuration:      configuration,
		Lifecycle:          LifecycleActive,
		InitialSyncPhase:   InitialSyncNotStarted,
		CollectionMappings: make(map[string]CollectionMapping),
		TransactionParts:   make(map[string]TransactionFragment),
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
	data, err := json.Marshal(s.state)
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
