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

// Package transport implements the client side of MongoDB member OP_MSG traffic.
package transport

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
	"github.com/golang/snappy"
	"github.com/klauspost/compress/zstd"
)

const compressedHeaderLength = 9

var uncompressedCommands = map[string]struct{}{
	"hello":           {},
	"isMaster":        {},
	"ismaster":        {},
	"saslStart":       {},
	"saslContinue":    {},
	"authenticate":    {},
	"getnonce":        {},
	"createUser":      {},
	"updateUser":      {},
	"copydbSaslStart": {},
	"copydbgetnonce":  {},
	"copydb":          {},
}

var requestID atomic.Int32

type Compressor byte

const (
	CompressorNoop   Compressor = 0
	CompressorSnappy Compressor = 1
	CompressorZlib   Compressor = 2
	CompressorZstd   Compressor = 3
)

func (c Compressor) String() string {
	switch c {
	case CompressorNoop:
		return "noop"
	case CompressorSnappy:
		return "snappy"
	case CompressorZlib:
		return "zlib"
	case CompressorZstd:
		return "zstd"
	default:
		return fmt.Sprintf("unknown(%d)", c)
	}
}

func compressorFromName(name string) (Compressor, bool) {
	switch name {
	case "noop":
		return CompressorNoop, true
	case "snappy":
		return CompressorSnappy, true
	case "zlib":
		return CompressorZlib, true
	case "zstd":
		return CompressorZstd, true
	default:
		return 0, false
	}
}

type Connection struct {
	connection net.Conn
	reader     *bufio.Reader
	writer     *bufio.Writer
	mu         sync.Mutex
	compressor Compressor
	checksums  bool
}

func Dial(ctx context.Context, address string) (*Connection, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial MongoDB member %q: %w", address, err)
	}
	return New(connection), nil
}

func New(connection net.Conn) *Connection {
	return &Connection{
		connection: connection,
		reader:     bufio.NewReader(connection),
		writer:     bufio.NewWriter(connection),
	}
}

func (c *Connection) Close() error {
	return c.connection.Close()
}

// Hello performs the unauthenticated member handshake. The response-selected
// compressor is used only after this request has completed.
func (c *Connection) Hello(ctx context.Context, compressors []string) (*wirebson.Document, error) {
	names := wirebson.MakeArray(len(compressors))
	for _, name := range compressors {
		if _, known := compressorFromName(name); !known {
			return nil, fmt.Errorf("unsupported requested compressor %q", name)
		}
		if err := names.Add(name); err != nil {
			return nil, fmt.Errorf("adding compressor %q: %w", name, err)
		}
	}

	message := wire.MustOpMsg(
		"hello", int32(1),
		"helloOk", true,
		"compression", names,
		"$db", "admin",
	)
	response, err := c.Request(ctx, message)
	if err != nil {
		return nil, err
	}
	document, err := response.RawDocument()
	if err != nil {
		return nil, fmt.Errorf("decoding hello response: %w", err)
	}
	decoded, err := document.Decode()
	if err != nil {
		return nil, fmt.Errorf("decoding hello response BSON: %w", err)
	}
	if ok, _ := decoded.Get("ok").(float64); ok != 1 {
		return nil, fmt.Errorf("MongoDB member hello failed: %v", decoded.Get("errmsg"))
	}
	offered, err := compressionArray(decoded.Get("compression"))
	if err != nil {
		return nil, err
	}
	if offered != nil {
		available := make(map[string]struct{}, offered.Len())
		for name := range offered.Values() {
			if value, ok := name.(string); ok {
				available[value] = struct{}{}
			}
		}
		for _, name := range compressors {
			if _, ok := available[name]; ok {
				c.compressor, _ = compressorFromName(name)
				break
			}
		}
	}
	if maxWireVersion, _ := decoded.Get("maxWireVersion").(int32); maxWireVersion >= 8 {
		c.checksums = true
	}
	return decoded, nil
}

func compressionArray(value any) (*wirebson.Array, error) {
	switch value := value.(type) {
	case nil:
		return nil, nil
	case *wirebson.Array:
		return value, nil
	case wirebson.RawArray:
		array, err := value.Decode()
		if err != nil {
			return nil, fmt.Errorf("decoding MongoDB member compressor list: %w", err)
		}
		return array, nil
	default:
		return nil, fmt.Errorf("MongoDB member returned invalid compressor list type %T", value)
	}
}

// Request serializes request/response pairs, checks response correlation, and
// honors the deadline in ctx for both halves of the exchange.
func (c *Connection) Request(ctx context.Context, message *wire.OpMsg) (*wire.OpMsg, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	identifier := requestID.Add(1)
	if err := c.writeMessage(ctx, identifier, message); err != nil {
		return nil, err
	}
	header, response, err := c.readMessage(ctx)
	if err != nil {
		return nil, err
	}
	if header.ResponseTo != identifier {
		return nil, fmt.Errorf("response correlation mismatch: got %d, want %d", header.ResponseTo, identifier)
	}
	return response, nil
}

// Exhaust sends an exhaust-enabled command and passes each response to consume.
// The callback returns false to stop consuming and close the connection; doing
// so is required because an exhaust stream owns the connection until completion.
func (c *Connection) Exhaust(ctx context.Context, message *wire.OpMsg, consume func(*wire.OpMsg) (bool, error)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !message.Flags.FlagSet(wire.OpMsgExhaustAllowed) {
		return errors.New("exhaust command must set OP_MSG exhaustAllowed")
	}
	identifier := requestID.Add(1)
	if err := c.writeMessage(ctx, identifier, message); err != nil {
		return err
	}
	for {
		header, response, err := c.readMessage(ctx)
		if err != nil {
			return err
		}
		if header.ResponseTo != identifier {
			return fmt.Errorf("exhaust response correlation mismatch: got %d, want %d", header.ResponseTo, identifier)
		}
		keepGoing, err := consume(response)
		if err != nil {
			return err
		}
		if !keepGoing && response.Flags.FlagSet(wire.OpMsgMoreToCome) {
			_ = c.Close()
			return nil
		}
		if !response.Flags.FlagSet(wire.OpMsgMoreToCome) {
			return nil
		}
	}
}

func (c *Connection) writeMessage(ctx context.Context, identifier int32, message *wire.OpMsg) error {
	body, err := message.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encoding OP_MSG: %w", err)
	}
	originalHeader := wire.MsgHeader{
		RequestID: identifier,
		OpCode:    wire.OpCodeMsg,
	}
	if c.checksums {
		body, err = appendChecksum(originalHeader, body)
		if err != nil {
			return err
		}
	}
	opcode := wire.OpCodeMsg
	payload := body
	compressible, err := commandCanCompress(message)
	if err != nil {
		return err
	}
	if c.compressor != CompressorNoop && compressible {
		compressed, err := compress(c.compressor, body)
		if err != nil {
			return err
		}
		payload = make([]byte, compressedHeaderLength+len(compressed))
		binary.LittleEndian.PutUint32(payload[0:4], uint32(wire.OpCodeMsg))
		binary.LittleEndian.PutUint32(payload[4:8], uint32(len(body)))
		payload[8] = byte(c.compressor)
		copy(payload[9:], compressed)
		opcode = wire.OpCodeCompressed
	}
	header := wire.MsgHeader{
		MessageLength: int32(wire.MsgHeaderLen + len(payload)),
		RequestID:     identifier,
		OpCode:        opcode,
	}
	if err := setWriteDeadline(ctx, c.connection); err != nil {
		return err
	}
	if err := writeHeader(c.writer, header); err != nil {
		return err
	}
	if _, err := c.writer.Write(payload); err != nil {
		return fmt.Errorf("writing MongoDB member message: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("flushing MongoDB member message: %w", err)
	}
	return nil
}

func appendChecksum(header wire.MsgHeader, body []byte) ([]byte, error) {
	if len(body) < 4 {
		return nil, errors.New("OP_MSG is shorter than its flags")
	}
	flags := wire.OpMsgFlags(binary.LittleEndian.Uint32(body[:4]))
	if flags.FlagSet(wire.OpMsgChecksumPresent) {
		return nil, errors.New("OP_MSG already contains a checksum")
	}
	body = append(body, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(body[:4], uint32(flags|wire.OpMsgFlags(wire.OpMsgChecksumPresent)))
	header.MessageLength = int32(wire.MsgHeaderLen + len(body))
	headerBytes, err := header.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("encoding OP_MSG checksum header: %w", err)
	}
	checksum := crc32.Checksum(append(headerBytes, body[:len(body)-4]...), crc32.MakeTable(crc32.Castagnoli))
	binary.LittleEndian.PutUint32(body[len(body)-4:], checksum)
	return body, nil
}

func commandCanCompress(message *wire.OpMsg) (bool, error) {
	document, err := message.RawSection0().Decode()
	if err != nil {
		return false, fmt.Errorf("decoding MongoDB command for compression: %w", err)
	}
	_, excluded := uncompressedCommands[document.Command()]
	return !excluded, nil
}

func (c *Connection) readMessage(ctx context.Context) (wire.MsgHeader, *wire.OpMsg, error) {
	if err := setReadDeadline(ctx, c.connection); err != nil {
		return wire.MsgHeader{}, nil, err
	}
	header, err := readHeader(c.reader)
	if err != nil {
		return wire.MsgHeader{}, nil, err
	}
	body := make([]byte, int(header.MessageLength)-wire.MsgHeaderLen)
	if _, err := io.ReadFull(c.reader, body); err != nil {
		return wire.MsgHeader{}, nil, fmt.Errorf("reading MongoDB member message body: %w", err)
	}
	if header.OpCode == wire.OpCodeCompressed {
		header.OpCode, body, err = decompress(body)
		if err != nil {
			return wire.MsgHeader{}, nil, err
		}
		header.MessageLength = int32(wire.MsgHeaderLen + len(body))
	}
	if header.OpCode != wire.OpCodeMsg {
		return wire.MsgHeader{}, nil, fmt.Errorf("unexpected MongoDB member response opcode %s", header.OpCode)
	}
	if err := validateChecksum(header, body); err != nil {
		return wire.MsgHeader{}, nil, err
	}
	var message wire.OpMsg
	if err := message.UnmarshalBinaryNocopy(body); err != nil {
		return wire.MsgHeader{}, nil, fmt.Errorf("decoding MongoDB member OP_MSG: %w", err)
	}
	return header, &message, nil
}

func readHeader(reader *bufio.Reader) (wire.MsgHeader, error) {
	var raw [wire.MsgHeaderLen]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return wire.MsgHeader{}, fmt.Errorf("reading MongoDB member message header: %w", err)
	}
	header := wire.MsgHeader{
		MessageLength: int32(binary.LittleEndian.Uint32(raw[0:4])),
		RequestID:     int32(binary.LittleEndian.Uint32(raw[4:8])),
		ResponseTo:    int32(binary.LittleEndian.Uint32(raw[8:12])),
		OpCode:        wire.OpCode(binary.LittleEndian.Uint32(raw[12:16])),
	}
	if header.MessageLength < wire.MsgHeaderLen || header.MessageLength > wire.MaxMsgLen {
		return wire.MsgHeader{}, fmt.Errorf("invalid MongoDB member message length %d", header.MessageLength)
	}
	return header, nil
}

func writeHeader(writer *bufio.Writer, header wire.MsgHeader) error {
	var raw [wire.MsgHeaderLen]byte
	binary.LittleEndian.PutUint32(raw[0:4], uint32(header.MessageLength))
	binary.LittleEndian.PutUint32(raw[4:8], uint32(header.RequestID))
	binary.LittleEndian.PutUint32(raw[8:12], uint32(header.ResponseTo))
	binary.LittleEndian.PutUint32(raw[12:16], uint32(header.OpCode))
	if _, err := writer.Write(raw[:]); err != nil {
		return fmt.Errorf("writing MongoDB member message header: %w", err)
	}
	return nil
}

func setReadDeadline(ctx context.Context, connection net.Conn) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Time{}
	}
	return connection.SetReadDeadline(deadline)
}

func setWriteDeadline(ctx context.Context, connection net.Conn) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Time{}
	}
	return connection.SetWriteDeadline(deadline)
}

func decompress(payload []byte) (wire.OpCode, []byte, error) {
	if len(payload) < compressedHeaderLength {
		return 0, nil, errors.New("compressed MongoDB message is shorter than its header")
	}
	opcode := wire.OpCode(binary.LittleEndian.Uint32(payload[0:4]))
	size := int(binary.LittleEndian.Uint32(payload[4:8]))
	if size < 0 || size > wire.MaxMsgLen-wire.MsgHeaderLen {
		return 0, nil, fmt.Errorf("invalid compressed MongoDB message size %d", size)
	}
	body, err := decompressBody(Compressor(payload[8]), payload[9:])
	if err != nil {
		return 0, nil, err
	}
	if len(body) != size {
		return 0, nil, fmt.Errorf("compressed MongoDB message decompressed to %d bytes, want %d", len(body), size)
	}
	return opcode, body, nil
}

func compress(compressor Compressor, body []byte) ([]byte, error) {
	switch compressor {
	case CompressorSnappy:
		return snappy.Encode(nil, body), nil
	case CompressorZlib:
		var buffer bytes.Buffer
		writer := zlib.NewWriter(&buffer)
		if _, err := writer.Write(body); err != nil {
			return nil, fmt.Errorf("compressing MongoDB message with zlib: %w", err)
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("closing MongoDB zlib compressor: %w", err)
		}
		return buffer.Bytes(), nil
	case CompressorZstd:
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			return nil, fmt.Errorf("creating MongoDB zstd compressor: %w", err)
		}
		defer encoder.Close()
		return encoder.EncodeAll(body, nil), nil
	default:
		return nil, fmt.Errorf("unsupported MongoDB compressor %s", compressor)
	}
}

func decompressBody(compressor Compressor, payload []byte) ([]byte, error) {
	switch compressor {
	case CompressorNoop:
		return append([]byte(nil), payload...), nil
	case CompressorSnappy:
		body, err := snappy.Decode(nil, payload)
		if err != nil {
			return nil, fmt.Errorf("decompressing MongoDB snappy message: %w", err)
		}
		return body, nil
	case CompressorZlib:
		reader, err := zlib.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("opening MongoDB zlib message: %w", err)
		}
		defer reader.Close()
		body, err := io.ReadAll(io.LimitReader(reader, wire.MaxMsgLen-wire.MsgHeaderLen+1))
		if err != nil {
			return nil, fmt.Errorf("decompressing MongoDB zlib message: %w", err)
		}
		return body, nil
	case CompressorZstd:
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(wire.MaxMsgLen))
		if err != nil {
			return nil, fmt.Errorf("creating MongoDB zstd decoder: %w", err)
		}
		defer decoder.Close()
		body, err := decoder.DecodeAll(payload, nil)
		if err != nil {
			return nil, fmt.Errorf("decompressing MongoDB zstd message: %w", err)
		}
		return body, nil
	default:
		return nil, fmt.Errorf("unsupported MongoDB compressor %s", compressor)
	}
}

func validateChecksum(header wire.MsgHeader, body []byte) error {
	if len(body) < 4 || wire.OpMsgFlags(binary.LittleEndian.Uint32(body[:4])).FlagSet(wire.OpMsgChecksumPresent) == false {
		return nil
	}
	if len(body) < 8 {
		return errors.New("checksummed MongoDB OP_MSG is shorter than flags and checksum")
	}
	want := binary.LittleEndian.Uint32(body[len(body)-4:])
	checksum := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	if err := binary.Write(checksum, binary.LittleEndian, header); err != nil {
		return fmt.Errorf("encoding MongoDB OP_MSG checksum header: %w", err)
	}
	if _, err := checksum.Write(body[:len(body)-4]); err != nil {
		return fmt.Errorf("calculating MongoDB OP_MSG checksum: %w", err)
	}
	if got := checksum.Sum32(); got != want {
		return errors.New("MongoDB OP_MSG checksum does not match contents")
	}
	return nil
}
