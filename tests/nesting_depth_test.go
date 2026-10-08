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
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func nestedD(depth int) bson.D {
	d := bson.D{}
	for range depth {
		d = bson.D{{Key: "a", Value: d}}
	}
	return d
}

// runRaw sends a pre-encoded OP_MSG section-0 body.
func (w *wireConn) runRaw(body []byte) (bson.M, error) {
	const opMsg = 2013
	reqID := w.next.Add(1)
	msgLen := 16 + 4 + 1 + len(body)
	buf := make([]byte, 21, msgLen)
	binary.LittleEndian.PutUint32(buf[0:], uint32(msgLen))
	binary.LittleEndian.PutUint32(buf[4:], uint32(reqID))
	binary.LittleEndian.PutUint32(buf[12:], opMsg)
	buf = append(buf, body...)

	if _, err := w.c.Write(buf); err != nil {
		return nil, err
	}
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(w.c, hdr); err != nil {
		return nil, err
	}
	rest := make([]byte, binary.LittleEndian.Uint32(hdr[0:])-16)
	if _, err := io.ReadFull(w.c, rest); err != nil {
		return nil, err
	}
	var out bson.M
	if err := bson.Unmarshal(rest[5:], &out); err != nil {
		return nil, err
	}
	return out, nil
}

// pingWithNesting encodes {ping: 1, a: {a: ...depth...}, $db: "admin"}
// without recursion so the client side cannot overflow its own stack.
func pingWithNesting(depth int) []byte {
	const levelOverhead = 4 + 3 + 1 // size, tag+name+NUL, terminator
	inner := make([]byte, 0, depth*levelOverhead+5)
	for level := range depth - 1 {
		inner = binary.LittleEndian.AppendUint32(inner, uint32((depth-level-1)*levelOverhead+5))
		inner = append(inner, 0x03, 'a', 0)
	}
	inner = append(inner, 5, 0, 0, 0, 0)
	for range depth - 1 {
		inner = append(inner, 0)
	}

	var body []byte
	body = append(body, 0, 0, 0, 0)
	body = append(body, 0x10, 'p', 'i', 'n', 'g', 0, 1, 0, 0, 0)
	body = append(body, 0x03, 'a', 0)
	body = append(body, inner...)
	body = append(body, 0x02, '$', 'd', 'b', 0, 6, 0, 0, 0, 'a', 'd', 'm', 'i', 'n', 0)
	body = append(body, 0)
	binary.LittleEndian.PutUint32(body, uint32(len(body)))
	return body
}

// Before the fix the server accepted any nesting depth, and converting a
// deep enough command overflowed the goroutine stack, a fatal error that
// recover() cannot catch, so one unauthenticated message killed the server.
func TestWire_ExcessiveNestingIsRejected(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("nesting_%d", env.Port)
	conn := dialWire(t, env)

	insert := func(id string, depth int) (bson.M, error) {
		return conn.run(bson.D{
			{Key: "insert", Value: "c"},
			{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: id}, {Key: "a", Value: nestedD(depth)}}}},
			{Key: "$db", Value: dbName},
		})
	}

	res, err := insert("deep", 250)
	require.NoError(t, err)
	assert.Equal(t, 0.0, res["ok"], "a command nested past the limit must be rejected: %v", res)
	assert.EqualValues(t, 15, res["code"], "%v", res)
	assert.Contains(t, res["errmsg"], "nested object depth")

	res, err = insert("shallow", 50)
	require.NoError(t, err, "the connection must remain usable")
	assert.Equal(t, 1.0, res["ok"], "%v", res)
	assert.EqualValues(t, 1, res["n"], "%v", res)
}

func TestWire_HugeNestingDoesNotCrashServer(t *testing.T) {
	env := startDumboDB(t)
	conn := dialWire(t, env)

	res, err := conn.runRaw(pingWithNesting(2_000_000))
	require.NoError(t, err, "the server must answer rather than crash")
	assert.Equal(t, 0.0, res["ok"])
	assert.EqualValues(t, 15, res["code"], "%v", res)

	fresh := dialWire(t, env)
	res, err = fresh.run(bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}})
	require.NoError(t, err)
	assert.Equal(t, 1.0, res["ok"])
}
