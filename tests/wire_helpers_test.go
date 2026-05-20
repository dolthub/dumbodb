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

package tests

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// wireConn speaks MongoDB OP_MSG directly to DumboDB so tests can stamp
// a chosen lsid onto a frame without the driver overriding it. Used by
// session-isolation tests that need to exercise lsid handling across
// two TCP connections sharing a forged-identical session id.
type wireConn struct {
	c    net.Conn
	next atomic.Int32
}

func dialWire(t *testing.T, env *dumboDBTestEnv) *wireConn {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", env.port)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	w := &wireConn{c: c}
	t.Cleanup(func() { _ = c.Close() })

	// Handshake -- some Mongo-compatible servers close the connection
	// without an opening hello. DumboDB tolerates either order; sending
	// hello unconditionally is the safer pattern.
	if _, err := w.run(bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}}); err != nil {
		t.Fatalf("wire hello: %v", err)
	}
	return w
}

func (w *wireConn) run(cmd bson.D) (bson.M, error) {
	body, err := bson.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	const opMsg = 2013
	reqID := w.next.Add(1)
	msgLen := 16 + 4 + 1 + len(body)
	buf := make([]byte, msgLen)
	binary.LittleEndian.PutUint32(buf[0:], uint32(msgLen))
	binary.LittleEndian.PutUint32(buf[4:], uint32(reqID))
	binary.LittleEndian.PutUint32(buf[8:], 0)
	binary.LittleEndian.PutUint32(buf[12:], opMsg)
	binary.LittleEndian.PutUint32(buf[16:], 0)
	buf[20] = 0
	copy(buf[21:], body)

	if _, err := w.c.Write(buf); err != nil {
		return nil, err
	}

	hdr := make([]byte, 16)
	if _, err := io.ReadFull(w.c, hdr); err != nil {
		return nil, err
	}
	rlen := binary.LittleEndian.Uint32(hdr[0:])
	rest := make([]byte, rlen-16)
	if _, err := io.ReadFull(w.c, rest); err != nil {
		return nil, err
	}
	var out bson.M
	if err := bson.Unmarshal(rest[5:], &out); err != nil {
		return nil, err
	}
	return out, nil
}

// freshLsid returns a UUID-shaped BSON document suitable for stamping
// into an OP_MSG frame's "lsid" field.
func freshLsid() bson.D {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return bson.D{{Key: "id", Value: bson.Binary{Subtype: 0x04, Data: id[:]}}}
}
