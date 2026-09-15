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
	"encoding/binary"
	"hash/crc32"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
)

func TestRequestCorrelatesResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		reader := bufio.NewReader(server)
		header, err := readHeader(reader)
		if err != nil {
			t.Error(err)
			return
		}
		if header.OpCode != wire.OpCodeMsg {
			t.Errorf("request opcode = %s", header.OpCode)
			return
		}
		if _, err := io.CopyN(io.Discard, reader, int64(header.MessageLength-wire.MsgHeaderLen)); err != nil {
			t.Error(err)
			return
		}
		writeTestMessage(t, server, 77, header.RequestID, wire.MustOpMsg("ok", float64(1)))
	}()

	connection := New(client)
	response, err := connection.Request(context.Background(), wire.MustOpMsg("ping", int32(1), "$db", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := response.RawDocument()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := document.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Get("ok") != float64(1) {
		t.Fatalf("response = %s", response)
	}
}

func TestHelloSelectsMutualCompressor(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		reader := bufio.NewReader(server)
		header, err := readHeader(reader)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := io.CopyN(io.Discard, reader, int64(header.MessageLength-wire.MsgHeaderLen)); err != nil {
			t.Error(err)
			return
		}
		writeTestMessage(t, server, 77, header.RequestID, wire.MustOpMsg(
			"ok", float64(1),
			"minWireVersion", int32(6),
			"maxWireVersion", int32(25),
			"compression", wirebson.MustArray("zstd", "snappy"),
		))
	}()

	connection := New(client)
	if _, err := connection.Hello(context.Background(), []string{"snappy", "zstd"}); err != nil {
		t.Fatal(err)
	}
	if connection.compressor != CompressorSnappy {
		t.Fatalf("compressor = %s, want snappy", connection.compressor)
	}
}

func TestMemberHelloAdvertisesInternalClient(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		reader := bufio.NewReader(server)
		header, err := readHeader(reader)
		if err != nil {
			t.Error(err)
			return
		}
		body := make([]byte, int(header.MessageLength)-wire.MsgHeaderLen)
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Error(err)
			return
		}
		var request wire.OpMsg
		if err := request.UnmarshalBinaryNocopy(body); err != nil {
			t.Error(err)
			return
		}
		raw, err := request.RawDocument()
		if err != nil {
			t.Error(err)
			return
		}
		document, err := raw.Decode()
		if err != nil {
			t.Error(err)
			return
		}
		if document.Get("internalClient") == nil {
			t.Error("member hello omitted internalClient")
		}
		if document.Get("client") == nil {
			t.Error("member hello omitted client metadata")
		}
		if hostInfo := document.Get("hostInfo"); hostInfo != "dumbo.example:27017" {
			t.Errorf("member hello hostInfo = %v", hostInfo)
		}
		writeTestMessage(t, server, 77, header.RequestID, wire.MustOpMsg(
			"ok", float64(1),
			"minWireVersion", int32(6),
			"maxWireVersion", int32(25),
		))
	}()

	connection := New(client)
	if _, err := connection.MemberHello(context.Background(), "dumbo.example:27017", nil); err != nil {
		t.Fatal(err)
	}
}

func TestMemberHelloRejectsIncompatibleWireRange(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		reader := bufio.NewReader(server)
		header, err := readHeader(reader)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := io.CopyN(io.Discard, reader, int64(header.MessageLength-wire.MsgHeaderLen)); err != nil {
			t.Error(err)
			return
		}
		writeTestMessage(t, server, 77, header.RequestID, wire.MustOpMsg(
			"ok", float64(1),
			"minWireVersion", int32(26),
			"maxWireVersion", int32(27),
		))
	}()
	connection := New(client)
	if _, err := connection.MemberHello(context.Background(), "dumbo.example:27017", nil); err == nil {
		t.Fatal("MemberHello accepted incompatible wire range")
	}
}

func TestRequestUsesNegotiatedCompression(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		reader := bufio.NewReader(server)
		header, err := readHeader(reader)
		if err != nil {
			t.Error(err)
			return
		}
		if header.OpCode != wire.OpCodeCompressed {
			t.Errorf("request opcode = %s, want OP_COMPRESSED", header.OpCode)
			return
		}
		payload := make([]byte, int(header.MessageLength)-wire.MsgHeaderLen)
		if _, err := io.ReadFull(reader, payload); err != nil {
			t.Error(err)
			return
		}
		opcode, body, err := decompress(payload)
		if err != nil {
			t.Error(err)
			return
		}
		if opcode != wire.OpCodeMsg {
			t.Errorf("compressed opcode = %s", opcode)
			return
		}
		var request wire.OpMsg
		if err := request.UnmarshalBinaryNocopy(body); err != nil {
			t.Error(err)
			return
		}
		writeTestMessage(t, server, 77, header.RequestID, wire.MustOpMsg("ok", float64(1)))
	}()

	connection := New(client)
	connection.compressor = CompressorSnappy
	if _, err := connection.Request(context.Background(), wire.MustOpMsg("ping", int32(1), "$db", "admin")); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticationCommandsAreNotCompressed(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		reader := bufio.NewReader(server)
		for range 2 {
			header, err := readHeader(reader)
			if err != nil {
				t.Error(err)
				return
			}
			if header.OpCode != wire.OpCodeMsg {
				t.Errorf("authentication request opcode = %s, want OP_MSG", header.OpCode)
				return
			}
			if _, err := io.CopyN(io.Discard, reader, int64(header.MessageLength-wire.MsgHeaderLen)); err != nil {
				t.Error(err)
				return
			}
			writeTestMessage(t, server, header.RequestID+1, header.RequestID, wire.MustOpMsg("ok", float64(1)))
		}
	}()

	connection := New(client)
	connection.compressor = CompressorSnappy
	for _, command := range []string{"saslStart", "saslContinue"} {
		if _, err := connection.Request(context.Background(), wire.MustOpMsg(command, int32(1), "$db", "admin")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRequestWritesChecksum(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		reader := bufio.NewReader(server)
		header, err := readHeader(reader)
		if err != nil {
			t.Error(err)
			return
		}
		body := make([]byte, int(header.MessageLength)-wire.MsgHeaderLen)
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Error(err)
			return
		}
		if err := validateChecksum(header, body); err != nil {
			t.Error(err)
			return
		}
		writeTestMessage(t, server, 77, header.RequestID, wire.MustOpMsg("ok", float64(1)))
	}()

	connection := New(client)
	connection.checksums = true
	if _, err := connection.Request(context.Background(), wire.MustOpMsg("find", "orders", "$db", "shop")); err != nil {
		t.Fatal(err)
	}
}

func TestCommandCompressionExclusions(t *testing.T) {
	for command := range uncompressedCommands {
		compressible, err := commandCanCompress(wire.MustOpMsg(command, int32(1), "$db", "admin"))
		if err != nil {
			t.Fatal(err)
		}
		if compressible {
			t.Fatalf("%s is compressible", command)
		}
	}
	compressible, err := commandCanCompress(wire.MustOpMsg("find", "orders", "$db", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	if !compressible {
		t.Fatal("find is not compressible")
	}
}

func TestExhaustConsumesAllResponses(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		reader := bufio.NewReader(server)
		header, err := readHeader(reader)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := io.CopyN(io.Discard, reader, int64(header.MessageLength-wire.MsgHeaderLen)); err != nil {
			t.Error(err)
			return
		}
		more := wire.MustOpMsg("n", int32(1), "ok", float64(1))
		more.Flags = wire.OpMsgFlags(wire.OpMsgMoreToCome)
		writeTestMessage(t, server, 88, header.RequestID, more)
		writeTestMessage(t, server, 89, 88, wire.MustOpMsg("n", int32(2), "ok", float64(1)))
	}()

	connection := New(client)
	request := wire.MustOpMsg("hello", int32(1), "$db", "admin")
	request.Flags = wire.OpMsgFlags(wire.OpMsgExhaustAllowed)
	var values []int32
	err := connection.Exhaust(context.Background(), request, func(response *wire.OpMsg) (bool, error) {
		raw, err := response.RawDocument()
		if err != nil {
			return false, err
		}
		document, err := raw.Decode()
		if err != nil {
			return false, err
		}
		values = append(values, document.Get("n").(int32))
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0] != 1 || values[1] != 2 {
		t.Fatalf("exhaust values = %v", values)
	}
}

func TestCompressedPayloadsRoundTrip(t *testing.T) {
	body := []byte("a MongoDB member message that is long enough to compress more than once")
	for _, compressor := range []Compressor{CompressorSnappy, CompressorZlib, CompressorZstd} {
		t.Run(compressor.String(), func(t *testing.T) {
			compressed, err := compress(compressor, body)
			if err != nil {
				t.Fatal(err)
			}
			payload := make([]byte, compressedHeaderLength+len(compressed))
			binary.LittleEndian.PutUint32(payload[0:4], uint32(wire.OpCodeMsg))
			binary.LittleEndian.PutUint32(payload[4:8], uint32(len(body)))
			payload[8] = byte(compressor)
			copy(payload[9:], compressed)
			opcode, got, err := decompress(payload)
			if err != nil {
				t.Fatal(err)
			}
			if opcode != wire.OpCodeMsg || string(got) != string(body) {
				t.Fatalf("decompressed opcode/body = %s/%q", opcode, got)
			}
		})
	}
}

func TestChecksumValidationRejectsTampering(t *testing.T) {
	message := wire.MustOpMsg("ping", int32(1), "$db", "admin")
	body, err := message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(body[0:4], uint32(wire.OpMsgChecksumPresent))
	body = append(body, 0, 0, 0, 0)
	header := wire.MsgHeader{MessageLength: int32(wire.MsgHeaderLen + len(body)), RequestID: 4, OpCode: wire.OpCodeMsg}
	checksum := checksumForTest(t, header, body[:len(body)-4])
	binary.LittleEndian.PutUint32(body[len(body)-4:], checksum)
	if err := validateChecksum(header, body); err != nil {
		t.Fatal(err)
	}
	body[5] ^= 0x01
	if err := validateChecksum(header, body); err == nil {
		t.Fatal("validateChecksum accepted a tampered message")
	}
}

func TestMalformedCompressedPayloadIsRejected(t *testing.T) {
	if _, _, err := decompress([]byte{1, 2}); err == nil {
		t.Fatal("decompress accepted a truncated header")
	}
	payload := make([]byte, compressedHeaderLength)
	binary.LittleEndian.PutUint32(payload[0:4], uint32(wire.OpCodeMsg))
	binary.LittleEndian.PutUint32(payload[4:8], 1)
	payload[8] = 127
	if _, _, err := decompress(payload); err == nil {
		t.Fatal("decompress accepted an unknown compressor")
	}
}

func TestLiveMongoHello(t *testing.T) {
	address := os.Getenv("MONGOD_ADDRESS")
	if address == "" {
		t.Skip("set MONGOD_ADDRESS to run against mongod")
	}
	connection, err := Dial(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	response, err := connection.Hello(context.Background(), []string{"snappy", "zlib", "zstd"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Get("ok") != float64(1) {
		t.Fatalf("hello response = %v", response)
	}
	if _, err := connection.Request(context.Background(), wire.MustOpMsg("ping", int32(1), "$db", "admin")); err != nil {
		t.Fatal(err)
	}
}

func TestLiveDumboAwaitableHello(t *testing.T) {
	address := os.Getenv("DUMBODB_ADDRESS")
	if address == "" {
		t.Skip("set DUMBODB_ADDRESS to run against DumboDB in replica-set mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := Dial(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	initial, err := connection.Hello(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	topologyVersion := initial.Get("topologyVersion")
	if topologyVersion == nil {
		t.Fatal("replica-set hello omitted topologyVersion")
	}
	request := wire.MustOpMsg(
		"hello", int32(1),
		"topologyVersion", topologyVersion,
		"maxAwaitTimeMS", int64(25),
		"$db", "admin",
	)
	request.Flags = wire.OpMsgFlags(wire.OpMsgExhaustAllowed)
	responses := 0
	if err := connection.Exhaust(ctx, request, func(response *wire.OpMsg) (bool, error) {
		responses++
		return responses < 2, nil
	}); err != nil {
		t.Fatal(err)
	}
	if responses != 2 {
		t.Fatalf("awaitable hello responses = %d, want 2", responses)
	}
}

func writeTestMessage(t *testing.T, connection net.Conn, requestID, responseTo int32, message *wire.OpMsg) {
	t.Helper()
	body, err := message.MarshalBinary()
	if err != nil {
		t.Error(err)
		return
	}
	header := wire.MsgHeader{MessageLength: int32(wire.MsgHeaderLen + len(body)), RequestID: requestID, ResponseTo: responseTo, OpCode: wire.OpCodeMsg}
	writer := bufio.NewWriter(connection)
	if err := writeHeader(writer, header); err != nil {
		t.Error(err)
		return
	}
	if _, err := writer.Write(body); err != nil {
		t.Error(err)
		return
	}
	if err := writer.Flush(); err != nil {
		t.Error(err)
	}
}

func checksumForTest(t *testing.T, header wire.MsgHeader, body []byte) uint32 {
	t.Helper()
	headerBytes, err := header.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	checksumData := append(headerBytes, body...)
	return crc32.Checksum(checksumData, crc32.MakeTable(crc32.Castagnoli))
}
