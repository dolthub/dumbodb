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

// Package topology tracks MongoDB replica-set configuration and member state.
package topology

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dolthub/dumbodb/internal/replication/control"
)

var ErrSourceRollbackIDChanged = errors.New("sync source rollback ID changed")

type MemberState int32

const (
	StateStartup    MemberState = 0
	StatePrimary    MemberState = 1
	StateSecondary  MemberState = 2
	StateRecovering MemberState = 3
	StateStartup2   MemberState = 5
	StateUnknown    MemberState = 6
	StateArbiter    MemberState = 7
	StateDown       MemberState = 8
	StateRollback   MemberState = 9
	StateRemoved    MemberState = 10
)

func (s MemberState) String() string {
	switch s {
	case StateStartup:
		return "STARTUP"
	case StatePrimary:
		return "PRIMARY"
	case StateSecondary:
		return "SECONDARY"
	case StateRecovering:
		return "RECOVERING"
	case StateStartup2:
		return "STARTUP2"
	case StateUnknown:
		return "UNKNOWN"
	case StateArbiter:
		return "ARBITER"
	case StateDown:
		return "DOWN"
	case StateRollback:
		return "ROLLBACK"
	case StateRemoved:
		return "REMOVED"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", s)
	}
}

type Heartbeat struct {
	SetName       string
	MemberID      int
	State         MemberState
	Term          int64
	PrimaryID     int
	Applied       control.OpTime
	Written       control.OpTime
	Durable       control.OpTime
	Committed     control.OpTime
	Configuration *control.ReplicaConfiguration
	ObservedAt    time.Time
}

type MemberStatus struct {
	MemberID      int
	Host          string
	State         MemberState
	Healthy       bool
	LastHeartbeat time.Time
	Applied       control.OpTime
	Written       control.OpTime
	Durable       control.OpTime
	Committed     control.OpTime
}

type Snapshot struct {
	SetName       string
	MemberHost    string
	MemberID      int
	State         MemberState
	Term          int64
	PrimaryID     int
	PrimaryHost   string
	SyncSource    string
	RBID          int64
	Configuration *control.ReplicaConfiguration
	Checkpoint    control.Checkpoint
	LastCommitted control.OpTime
	Members       map[int]MemberStatus
}

type Manager struct {
	mu         sync.RWMutex
	store      *control.Store
	state      Snapshot
	onChange   func()
	onProgress func()
}

func New(store *control.Store) *Manager {
	persisted := store.Snapshot()
	state := Snapshot{
		SetName:    persisted.Configuration.SetName,
		MemberHost: persisted.Configuration.MemberHost,
		State:      StateStartup,
		PrimaryID:  -1,
		RBID:       persisted.CurrentRBID,
		SyncSource: persisted.CurrentSource,
		Checkpoint: persisted.Checkpoint,
		Members:    make(map[int]MemberStatus),
	}
	if persisted.Identity != nil {
		state.MemberID = persisted.Identity.MemberID
		state.Term = persisted.Identity.Term
	}
	if persisted.ReplicaConfig != nil {
		configuration := *persisted.ReplicaConfig
		configuration.Members = append([]control.MemberConfiguration(nil), persisted.ReplicaConfig.Members...)
		configuration.RawBSON = append([]byte(nil), persisted.ReplicaConfig.RawBSON...)
		state.Configuration = &configuration
		state.State = StateStartup2
		if persisted.InitialSyncPhase == control.InitialSyncComplete {
			state.State = StateSecondary
		}
	}
	if persisted.Lifecycle == control.LifecycleDetached {
		state.State = StateRemoved
	} else if persisted.PendingRollback != nil {
		state.State = StateRecovering
	}
	return &Manager{store: store, state: state}
}

func (m *Manager) SetChangeListener(listener func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = listener
}

func (m *Manager) SetProgressListener(listener func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onProgress = listener
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneSnapshot(m.state)
}

func (m *Manager) InstallConfiguration(configuration control.ReplicaConfiguration, term int64) error {
	m.mu.RLock()
	current := cloneConfiguration(m.state.Configuration)
	m.mu.RUnlock()
	if current != nil {
		if configuration.SetName != current.SetName || configuration.ReplicaSetID != current.ReplicaSetID {
			return errors.New("replica configuration identity changed")
		}
		comparison := compareConfiguration(configuration, *current)
		if comparison < 0 {
			return fmt.Errorf("refusing stale replica configuration term %d version %d", configuration.Term, configuration.Version)
		}
		if comparison == 0 {
			if err := m.store.ObserveTerm(term); err != nil {
				return err
			}
			m.mu.Lock()
			previous := cloneSnapshot(m.state)
			m.state.Term = max(m.state.Term, term)
			listener := m.changeListenerLocked(previous)
			m.mu.Unlock()
			if listener != nil {
				listener()
			}
			return nil
		}
	}
	if err := m.store.InstallReplicaConfiguration(configuration, term); err != nil {
		return err
	}
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	m.state.Configuration = cloneConfiguration(&configuration)
	m.state.Term = max(m.state.Term, term)
	configuredMembers := make(map[int]MemberStatus, len(configuration.Members))
	for _, member := range configuration.Members {
		if member.Host == m.state.MemberHost {
			m.state.MemberID = member.MemberID
			continue
		}
		status, exists := m.state.Members[member.MemberID]
		status.MemberID = member.MemberID
		status.Host = member.Host
		if !exists {
			status.State = StateUnknown
		}
		configuredMembers[member.MemberID] = status
	}
	m.state.Members = configuredMembers
	if m.state.State == StateStartup {
		m.state.State = StateStartup2
	}
	listener := m.changeListenerLocked(previous)
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

func (m *Manager) ObserveHeartbeat(host string, heartbeat Heartbeat) error {
	if heartbeat.SetName != m.state.SetName {
		return fmt.Errorf("heartbeat set %q does not match %q", heartbeat.SetName, m.state.SetName)
	}
	if heartbeat.Configuration != nil {
		if err := m.InstallConfiguration(*heartbeat.Configuration, heartbeat.Term); err != nil {
			return err
		}
	}
	if err := m.store.ObserveTerm(heartbeat.Term); err != nil {
		return err
	}
	if heartbeat.ObservedAt.IsZero() {
		heartbeat.ObservedAt = time.Now()
	}
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	m.state.Term = max(m.state.Term, heartbeat.Term)
	m.state.Members[heartbeat.MemberID] = MemberStatus{
		MemberID:      heartbeat.MemberID,
		Host:          host,
		State:         heartbeat.State,
		Healthy:       true,
		LastHeartbeat: heartbeat.ObservedAt,
		Applied:       heartbeat.Applied,
		Written:       heartbeat.Written,
		Durable:       heartbeat.Durable,
		Committed:     heartbeat.Committed,
	}
	if heartbeat.State == StatePrimary {
		m.state.PrimaryID = heartbeat.MemberID
		m.state.PrimaryHost = host
	} else if heartbeat.PrimaryID >= 0 {
		m.state.PrimaryID = heartbeat.PrimaryID
		m.state.PrimaryHost = m.memberHostLocked(heartbeat.PrimaryID)
	} else if m.state.PrimaryID == heartbeat.MemberID {
		m.state.PrimaryID = -1
		m.state.PrimaryHost = ""
	}
	m.state.SyncSource = m.selectSourceLocked()
	if m.state.SyncSource != previous.SyncSource {
		m.state.RBID = 0
		m.state.LastCommitted = control.OpTime{}
		if err := m.store.SetSource(m.state.SyncSource, 0); err != nil {
			m.state = previous
			m.mu.Unlock()
			return err
		}
	}
	if host == m.state.SyncSource && heartbeat.Committed != (control.OpTime{}) {
		m.state.LastCommitted = heartbeat.Committed
	}
	listener := m.changeListenerLocked(previous)
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

func (m *Manager) ObserveMemberContact(host string, memberID int, term int64, primaryID int) error {
	m.mu.RLock()
	hasConfiguration := m.state.Configuration != nil
	m.mu.RUnlock()
	if hasConfiguration {
		if err := m.store.ObserveTerm(term); err != nil {
			return err
		}
	}
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	m.state.Term = max(m.state.Term, term)
	status, exists := m.state.Members[memberID]
	status.MemberID = memberID
	status.Host = host
	status.Healthy = true
	status.LastHeartbeat = time.Now()
	if !exists {
		status.State = StateUnknown
	}
	m.state.Members[memberID] = status
	if primaryID >= 0 {
		m.state.PrimaryID = primaryID
		m.state.PrimaryHost = m.memberHostLocked(primaryID)
	}
	m.state.SyncSource = m.selectSourceLocked()
	if m.state.SyncSource != previous.SyncSource {
		m.state.RBID = 0
		m.state.LastCommitted = control.OpTime{}
		if err := m.store.SetSource(m.state.SyncSource, 0); err != nil {
			m.state = previous
			m.mu.Unlock()
			return err
		}
	}
	listener := m.changeListenerLocked(previous)
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

func (m *Manager) MarkInitialSyncComplete(checkpoint control.Checkpoint) error {
	if err := m.store.SetCheckpoint(checkpoint); err != nil {
		return err
	}
	if err := m.store.SetInitialSyncPhase(control.InitialSyncComplete); err != nil {
		return err
	}
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	m.state.Checkpoint = checkpoint
	m.state.State = StateSecondary
	listener := m.changeListenerLocked(previous)
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

// CompleteInitialSync atomically validates the attempt and publishes its durable stop position.
func (m *Manager) CompleteInitialSync(attemptID string, checkpoint control.Checkpoint, finalRBID int64) error {
	if err := m.store.CompleteInitialSync(attemptID, checkpoint, finalRBID); err != nil {
		return err
	}
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	m.state.Checkpoint = checkpoint
	m.state.RBID = finalRBID
	m.state.State = StateSecondary
	listener := m.changeListenerLocked(previous)
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

// ResetInitialSync clears failed-attempt progress and returns the member to STARTUP2.
func (m *Manager) ResetInitialSync(attemptID string) error {
	if err := m.store.ResetInitialSync(attemptID); err != nil {
		return err
	}
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	m.state.Checkpoint = control.Checkpoint{}
	m.state.RBID = 0
	m.state.State = StateStartup2
	listener := m.changeListenerLocked(previous)
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

func (m *Manager) MarkMemberDown(memberID int) error {
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	status, ok := m.state.Members[memberID]
	if !ok {
		m.mu.Unlock()
		return errors.New("unknown replica-set member")
	}
	status.Healthy = false
	status.State = StateDown
	m.state.Members[memberID] = status
	if m.state.PrimaryID == memberID {
		m.state.PrimaryID = -1
		m.state.PrimaryHost = ""
	}
	m.state.SyncSource = m.selectSourceLocked()
	if m.state.SyncSource != previous.SyncSource {
		m.state.RBID = 0
		m.state.LastCommitted = control.OpTime{}
		if err := m.store.SetSource(m.state.SyncSource, 0); err != nil {
			m.state = previous
			m.mu.Unlock()
			return err
		}
	}
	listener := m.changeListenerLocked(previous)
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

func (m *Manager) ObserveSourceRBID(host string, rbid int64) error {
	if rbid < 0 {
		return errors.New("sync source rollback ID cannot be negative")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if host != m.state.SyncSource {
		return fmt.Errorf("rollback ID came from %q, current sync source is %q", host, m.state.SyncSource)
	}
	if m.state.RBID != 0 && m.state.RBID != rbid {
		return fmt.Errorf("%w for %q: got %d, expected %d", ErrSourceRollbackIDChanged, host, rbid, m.state.RBID)
	}
	if m.state.RBID == rbid {
		return nil
	}
	if err := m.store.SetSource(host, rbid); err != nil {
		return err
	}
	m.state.RBID = rbid
	return nil
}

func (m *Manager) AdvanceFetched(fetched, buffered control.OpTime) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	checkpoint := m.state.Checkpoint
	checkpoint.Fetched = fetched
	checkpoint.Buffered = buffered
	if err := m.store.SetCheckpoint(checkpoint); err != nil {
		return err
	}
	m.state.Checkpoint = checkpoint
	return nil
}

// PublishCommit atomically persists commit provenance and reportable progress.
func (m *Manager) PublishCommit(interval control.CommitInterval, checkpoint control.Checkpoint) error {
	m.mu.Lock()
	if err := m.store.PublishCommit(interval, checkpoint); err != nil {
		m.mu.Unlock()
		return err
	}
	m.state.Checkpoint = checkpoint
	listener := m.onProgress
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

func (m *Manager) ResetFetchProgress() error {
	checkpoint, err := m.store.ResetFetchProgress()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.state.Checkpoint = checkpoint
	m.mu.Unlock()
	return nil
}

func (m *Manager) MarkContinuityLost(tooStale bool) error {
	if tooStale {
		if err := m.store.SetInitialSyncPhase(control.InitialSyncNotStarted); err != nil {
			return err
		}
	}
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	if tooStale {
		m.state.State = StateStartup2
	} else {
		m.state.State = StateRecovering
	}
	listener := m.changeListenerLocked(previous)
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

// MarkSteady returns a successfully publishing initialized member to SECONDARY.
func (m *Manager) MarkSteady() error {
	if m.store.Snapshot().InitialSyncPhase != control.InitialSyncComplete {
		return errors.New("member cannot become secondary before initial sync completes")
	}
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	m.state.State = StateSecondary
	listener := m.changeListenerLocked(previous)
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	return nil
}

func (m *Manager) CompleteRollback(commitID, source string, rbid int64) error {
	checkpoint, err := m.store.RollbackTo(commitID, source, rbid)
	if err != nil {
		return err
	}
	m.mu.Lock()
	previous := cloneSnapshot(m.state)
	m.state.Checkpoint = checkpoint
	m.state.SyncSource = source
	m.state.RBID = rbid
	m.state.State = StateSecondary
	listener := m.changeListenerLocked(previous)
	progressListener := m.onProgress
	m.mu.Unlock()
	if listener != nil {
		listener()
	}
	if progressListener != nil {
		progressListener()
	}
	return nil
}

func (m *Manager) selectSourceLocked() string {
	if primary, ok := m.state.Members[m.state.PrimaryID]; ok && primary.Healthy {
		return primary.Host
	}
	var selected MemberStatus
	for _, member := range m.state.Members {
		if !member.Healthy || member.State != StateSecondary || member.Host == m.state.MemberHost {
			continue
		}
		if selected.Host == "" || member.Applied.Compare(selected.Applied) > 0 {
			selected = member
		}
	}
	return selected.Host
}

func (m *Manager) memberHostLocked(memberID int) string {
	if status, ok := m.state.Members[memberID]; ok {
		return status.Host
	}
	if m.state.Configuration != nil {
		for _, member := range m.state.Configuration.Members {
			if member.MemberID == memberID {
				return member.Host
			}
		}
	}
	return ""
}

func (m *Manager) changeListenerLocked(previous Snapshot) func() {
	if previous.State == m.state.State && previous.Term == m.state.Term && previous.PrimaryID == m.state.PrimaryID && previous.SyncSource == m.state.SyncSource && configurationVersion(previous.Configuration) == configurationVersion(m.state.Configuration) {
		return nil
	}
	return m.onChange
}

func configurationVersion(configuration *control.ReplicaConfiguration) [2]int64 {
	if configuration == nil {
		return [2]int64{-1, -1}
	}
	return [2]int64{configuration.Term, configuration.Version}
}

func compareConfiguration(left, right control.ReplicaConfiguration) int {
	if left.Term != right.Term {
		if left.Term < right.Term {
			return -1
		}
		return 1
	}
	if left.Version < right.Version {
		return -1
	}
	if left.Version > right.Version {
		return 1
	}
	return 0
}

func cloneConfiguration(configuration *control.ReplicaConfiguration) *control.ReplicaConfiguration {
	if configuration == nil {
		return nil
	}
	clone := *configuration
	clone.Members = append([]control.MemberConfiguration(nil), configuration.Members...)
	clone.RawBSON = append([]byte(nil), configuration.RawBSON...)
	return &clone
}

func cloneSnapshot(state Snapshot) Snapshot {
	clone := state
	clone.Configuration = cloneConfiguration(state.Configuration)
	clone.Members = make(map[int]MemberStatus, len(state.Members))
	for id, member := range state.Members {
		clone.Members[id] = member
	}
	return clone
}
