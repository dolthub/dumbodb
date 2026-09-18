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
	"bufio"
	"context"
	"io"
	"net"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
)

func TestConnectorReplacesFailedConnection(t *testing.T) {
	firstClient, firstServer := net.Pipe()
	secondClient, secondServer := net.Pipe()
	defer firstServer.Close()
	defer secondServer.Close()

	connections := []net.Conn{firstClient, secondClient}
	dialCount := 0
	connector := newConnector("member.example:27017", []string{"snappy"}, func(context.Context, string) (net.Conn, error) {
		connection := connections[dialCount]
		dialCount++
		return connection, nil
	})

	firstClosed := make(chan struct{})
	go func() {
		serveConnectorHello(t, firstServer)
		var oneByte [1]byte
		_, _ = firstServer.Read(oneByte[:])
		close(firstClosed)
	}()
	first, err := connector.Connection(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	go serveConnectorHello(t, secondServer)
	second, err := connector.Replace(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("Replace returned the failed connection")
	}
	<-firstClosed

	current, err := connector.Replace(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if current != second || dialCount != 2 {
		t.Fatalf("concurrent-style Replace returned %p with %d dials, want %p with 2 dials", current, dialCount, second)
	}
	if err := connector.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Connection(context.Background()); err == nil {
		t.Fatal("closed Connector returned a connection")
	}
}

func TestConnectorClosesFailedHandshake(t *testing.T) {
	client, server := net.Pipe()
	connector := newConnector("member.example:27017", nil, func(context.Context, string) (net.Conn, error) {
		return client, nil
	})
	go func() {
		_ = server.Close()
	}()
	if _, err := connector.Connection(context.Background()); err == nil {
		t.Fatal("Connection accepted a failed handshake")
	}
	if connector.connection != nil {
		t.Fatal("Connector retained a failed handshake connection")
	}
}

func serveConnectorHello(t *testing.T, connection net.Conn) {
	t.Helper()
	reader := bufio.NewReader(connection)
	header, err := readHeader(reader)
	if err != nil {
		t.Error(err)
		return
	}
	if _, err := io.CopyN(io.Discard, reader, int64(header.MessageLength-wire.MsgHeaderLen)); err != nil {
		t.Error(err)
		return
	}
	writeTestMessage(t, connection, header.RequestID+1, header.RequestID, wire.MustOpMsg(
		"ok", float64(1),
		"maxWireVersion", int32(25),
		"compression", wirebson.MustArray("snappy"),
	))
}
