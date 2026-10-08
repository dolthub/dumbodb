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

package clientconn

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
	"github.com/stretchr/testify/require"
)

// Before the fix reading a message allocated its full declared length
// (up to 48MB) as soon as the 16-byte header arrived, so idle connections
// that sent only a header could pin gigabytes.
func TestReadMessageAllocatesForReceivedBytesOnly(t *testing.T) {
	header := make([]byte, wire.MsgHeaderLen)
	binary.LittleEndian.PutUint32(header[0:], uint32(wire.MaxMsgLen))
	binary.LittleEndian.PutUint32(header[12:], uint32(wire.OpCodeMsg))
	input := append(header, make([]byte, 4096)...)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err := readMessage(bufio.NewReader(bytes.NewReader(input)))
	runtime.ReadMemStats(&after)

	require.Error(t, err, "a truncated message is an error")
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8<<20), "allocation must follow received bytes, not the declared length")
}

func TestReadMessageRoundTrips(t *testing.T) {
	for _, size := range []int{10, 2 << 20} {
		msg := wire.MustOpMsg("insert", "c", "blob", string(make([]byte, size)), "$db", "x")
		header := &wire.MsgHeader{OpCode: wire.OpCodeMsg, RequestID: 7}
		raw, err := msg.MarshalBinary()
		require.NoError(t, err)
		header.MessageLength = int32(wire.MsgHeaderLen + len(raw))
		hb, err := header.MarshalBinary()
		require.NoError(t, err)

		gotHeader, gotBody, err := readMessage(bufio.NewReader(bytes.NewReader(append(hb, raw...))))
		require.NoError(t, err)
		require.Equal(t, int32(7), gotHeader.RequestID)
		doc, err := gotBody.(*wire.OpMsg).RawDocument()
		require.NoError(t, err)
		decoded, err := doc.Decode()
		require.NoError(t, err)
		require.Equal(t, "c", decoded.Get("insert"))
		_ = wirebson.Binary{}
	}
}
