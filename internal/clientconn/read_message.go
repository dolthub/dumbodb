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
	"io"
	"time"

	"github.com/FerretDB/wire"
)

// handshakeTimeout bounds TLS negotiation and, for optional TLS, the wait for
// the first bytes that select the protocol.
const handshakeTimeout = 30 * time.Second

// largeMessageThreshold is the declared length above which a message is read
// as its bytes arrive rather than into a buffer of the declared length.
const largeMessageThreshold = 1 << 20

// readMessage reads one wire message. wire.ReadMessage allocates the full
// declared length (up to wire.MaxMsgLen) as soon as the header arrives, so a
// client sending only headers could pin that much per connection. Large
// messages are first read into a buffer that grows with the bytes actually
// received.
func readMessage(r *bufio.Reader) (*wire.MsgHeader, wire.MsgBody, error) {
	prefix, err := r.Peek(4)
	if err != nil {
		return wire.ReadMessage(r)
	}

	length := int64(binary.LittleEndian.Uint32(prefix))
	if length <= largeMessageThreshold || length > wire.MaxMsgLen {
		return wire.ReadMessage(r)
	}

	var buf bytes.Buffer
	if _, err = io.CopyN(&buf, r, length); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, nil, err
	}

	return wire.ReadMessage(bufio.NewReader(&buf))
}
