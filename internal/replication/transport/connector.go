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

package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/dolthub/dumbodb/internal/replication/membership"
)

type dialFunc func(context.Context, string) (net.Conn, error)

type Connector struct {
	mu          sync.Mutex
	address     string
	hostInfo    string
	compressors []string
	dial        dialFunc
	connection  *Connection
	credentials *membership.Credentials
	authMode    membership.AuthMode
	closed      bool
}

type MemberOptions struct {
	Credentials       *membership.Credentials
	TLSConfig         *tls.Config
	TLSConfigProvider func() *tls.Config
	AuthMode          membership.AuthMode
}

func NewConnector(address string, compressors []string) *Connector {
	return NewMemberConnector(address, "", compressors)
}

func NewMemberConnector(address, hostInfo string, compressors []string, options ...MemberOptions) *Connector {
	var configured MemberOptions
	if len(options) != 0 {
		configured = options[0]
	}
	var dialer dialFunc
	if configured.TLSConfig != nil || configured.TLSConfigProvider != nil {
		dialer = func(ctx context.Context, address string) (net.Conn, error) {
			config := configured.TLSConfig
			if configured.TLSConfigProvider != nil {
				config = configured.TLSConfigProvider()
			}
			tlsDialer := &tls.Dialer{NetDialer: &net.Dialer{}, Config: config.Clone()}
			return tlsDialer.DialContext(ctx, "tcp", address)
		}
	} else {
		networkDialer := &net.Dialer{}
		dialer = func(ctx context.Context, address string) (net.Conn, error) {
			return networkDialer.DialContext(ctx, "tcp", address)
		}
	}
	connector := newConnector(address, compressors, func(ctx context.Context, address string) (net.Conn, error) {
		return dialer(ctx, address)
	})
	connector.hostInfo = hostInfo
	connector.credentials = configured.Credentials
	connector.authMode = configured.AuthMode
	return connector
}

func newConnector(address string, compressors []string, dial dialFunc) *Connector {
	return &Connector{
		address:     address,
		compressors: append([]string(nil), compressors...),
		dial:        dial,
	}
}

func (c *Connector) Connection(ctx context.Context) (*Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("MongoDB member connector is closed")
	}
	if c.connection != nil {
		return c.connection, nil
	}
	return c.connectLocked(ctx)
}

func (c *Connector) Replace(ctx context.Context, failed *Connection) (*Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("MongoDB member connector is closed")
	}
	if c.connection != nil && c.connection != failed {
		return c.connection, nil
	}
	if c.connection != nil {
		_ = c.connection.Close()
		c.connection = nil
	}
	return c.connectLocked(ctx)
}

func (c *Connector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.connection == nil {
		return nil
	}
	err := c.connection.Close()
	c.connection = nil
	return err
}

func (c *Connector) connectLocked(ctx context.Context) (*Connection, error) {
	networkConnection, err := c.dial(ctx, c.address)
	if err != nil {
		return nil, fmt.Errorf("dial MongoDB member %q: %w", c.address, err)
	}
	connection := New(networkConnection)
	if _, err := connection.MemberHello(ctx, c.hostInfo, c.compressors); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("handshake with MongoDB member %q: %w", c.address, err)
	}
	if c.authMode.SendsX509() {
		if err := connection.AuthenticateMemberX509(ctx); err != nil {
			_ = connection.Close()
			return nil, fmt.Errorf("authenticate with MongoDB member %q: %w", c.address, err)
		}
	} else if c.credentials != nil {
		if err := connection.AuthenticateMember(ctx, c.credentials); err != nil {
			_ = connection.Close()
			return nil, fmt.Errorf("authenticate with MongoDB member %q: %w", c.address, err)
		}
	}
	c.connection = connection
	return connection, nil
}
