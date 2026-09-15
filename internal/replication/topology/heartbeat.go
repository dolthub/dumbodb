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

package topology

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/replication/transport"
)

const (
	defaultHeartbeatInterval = 2 * time.Second
	defaultHeartbeatTimeout  = 10 * time.Second
)

type heartbeatClient interface {
	Request(context.Context, *wire.OpMsg) (*wire.OpMsg, error)
	Close() error
}

type connectorClient struct {
	connector *transport.Connector
}

func newConnectorClient(host, memberHost string) heartbeatClient {
	return &connectorClient{connector: transport.NewMemberConnector(host, memberHost, []string{"snappy", "zstd", "zlib"})}
}

func (c *connectorClient) Request(ctx context.Context, request *wire.OpMsg) (*wire.OpMsg, error) {
	connection, err := c.connector.Connection(ctx)
	if err != nil {
		return nil, err
	}
	response, err := connection.Request(ctx, request)
	if err == nil {
		return response, nil
	}
	_, _ = c.connector.Replace(ctx, connection)
	return nil, err
}

func (c *connectorClient) Close() error {
	return c.connector.Close()
}

type heartbeatPeer struct {
	memberID    int
	client      heartbeatClient
	firstSeen   time.Time
	lastSuccess time.Time
}

// HeartbeatMesh exchanges topology heartbeats with every known replica-set peer.
type HeartbeatMesh struct {
	manager   *Manager
	logger    *slog.Logger
	interval  time.Duration
	timeout   time.Duration
	newClient func(string) heartbeatClient
	now       func() time.Time

	mu    sync.Mutex
	peers map[string]*heartbeatPeer
}

func NewHeartbeatMesh(manager *Manager, logger *slog.Logger) *HeartbeatMesh {
	if logger == nil {
		logger = slog.Default()
	}
	return &HeartbeatMesh{
		manager:  manager,
		logger:   logger,
		interval: defaultHeartbeatInterval,
		timeout:  defaultHeartbeatTimeout,
		newClient: func(host string) heartbeatClient {
			return newConnectorClient(host, manager.Snapshot().MemberHost)
		},
		now:   time.Now,
		peers: make(map[string]*heartbeatPeer),
	}
}

func (m *HeartbeatMesh) Run(ctx context.Context) {
	defer m.close()
	m.poll(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll(ctx)
		}
	}
}

func (m *HeartbeatMesh) poll(ctx context.Context) {
	state := m.manager.Snapshot()
	targets := heartbeatTargets(state)
	peers := m.reconcilePeers(targets)
	var wait sync.WaitGroup
	for host, peer := range peers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			m.pollPeer(ctx, state, host, peer)
		}()
	}
	wait.Wait()
}

func (m *HeartbeatMesh) pollPeer(parent context.Context, state Snapshot, host string, peer *heartbeatPeer) {
	ctx, cancel := context.WithTimeout(parent, m.timeout)
	defer cancel()
	response, err := peer.client.Request(ctx, heartbeatRequest(state))
	observedAt := m.now()
	if err == nil {
		var heartbeat Heartbeat
		heartbeat, err = parseHeartbeatResponse(response, state.SetName, peer.memberID, observedAt)
		if err == nil {
			err = m.manager.ObserveHeartbeat(host, heartbeat)
		}
	}
	if err == nil {
		m.mu.Lock()
		peer.lastSuccess = observedAt
		m.mu.Unlock()
		return
	}
	m.logger.Warn("replica-set heartbeat failed", "host", host, "err", err)
	m.mu.Lock()
	lastContact := peer.lastSuccess
	if lastContact.IsZero() {
		lastContact = peer.firstSeen
	}
	m.mu.Unlock()
	if observedAt.Sub(lastContact) >= m.timeout {
		if markErr := m.manager.MarkMemberDown(peer.memberID); markErr != nil {
			m.logger.Debug("could not mark heartbeat peer down", "host", host, "err", markErr)
		}
	}
}

func heartbeatTargets(state Snapshot) map[string]int {
	targets := make(map[string]int)
	if state.Configuration != nil {
		for _, member := range state.Configuration.Members {
			if member.Host != state.MemberHost {
				targets[member.Host] = member.MemberID
			}
		}
		return targets
	}
	for memberID, member := range state.Members {
		if member.Host != "" && member.Host != state.MemberHost {
			targets[member.Host] = memberID
		}
	}
	return targets
}

func (m *HeartbeatMesh) reconcilePeers(targets map[string]int) map[string]*heartbeatPeer {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for host, memberID := range targets {
		peer := m.peers[host]
		if peer == nil {
			peer = &heartbeatPeer{memberID: memberID, client: m.newClient(host), firstSeen: now}
			m.peers[host] = peer
		} else {
			peer.memberID = memberID
		}
	}
	for host, peer := range m.peers {
		if _, ok := targets[host]; ok {
			continue
		}
		_ = peer.client.Close()
		delete(m.peers, host)
	}
	result := make(map[string]*heartbeatPeer, len(m.peers))
	for host, peer := range m.peers {
		result[host] = peer
	}
	return result
}

func (m *HeartbeatMesh) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for host, peer := range m.peers {
		_ = peer.client.Close()
		delete(m.peers, host)
	}
}
