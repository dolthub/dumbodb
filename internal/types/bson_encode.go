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

package types

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"time"
)

// AppendBSON appends the document's BSON encoding to dst. Size dst with BSONSize
// to encode without reallocating.
func (d *Document) AppendBSON(dst []byte) []byte {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0)
	for _, f := range d.fields {
		tagAt := len(dst)
		dst = append(dst, 0)
		dst = append(dst, f.key...)
		dst = append(dst, 0)
		dst = appendBSONValue(dst, tagAt, f.value)
	}
	dst = append(dst, 0)
	binary.LittleEndian.PutUint32(dst[start:], uint32(len(dst)-start))
	return dst
}

func (a *Array) appendBSON(dst []byte) []byte {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0)
	for i, v := range a.s {
		tagAt := len(dst)
		dst = append(dst, 0)
		dst = strconv.AppendInt(dst, int64(i), 10)
		dst = append(dst, 0)
		dst = appendBSONValue(dst, tagAt, v)
	}
	dst = append(dst, 0)
	binary.LittleEndian.PutUint32(dst[start:], uint32(len(dst)-start))
	return dst
}

// appendBSONValue appends v's encoding to dst and stores its type tag at dst[tagAt].
func appendBSONValue(dst []byte, tagAt int, v any) []byte {
	var tag byte
	switch v := v.(type) {
	case *Document:
		tag = 0x03
		dst = v.AppendBSON(dst)
	case *Array:
		tag = 0x04
		dst = v.appendBSON(dst)
	case float64:
		tag = 0x01
		dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(v))
	case string:
		tag = 0x02
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(v)+1))
		dst = append(dst, v...)
		dst = append(dst, 0)
	case Binary:
		tag = 0x05
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(v.B)))
		dst = append(dst, byte(v.Subtype))
		dst = append(dst, v.B...)
	case ObjectID:
		tag = 0x07
		dst = append(dst, v[:]...)
	case bool:
		tag = 0x08
		if v {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}
	case time.Time:
		tag = 0x09
		dst = binary.LittleEndian.AppendUint64(dst, uint64(v.UnixMilli()))
	case NullType:
		tag = 0x0a
	case Regex:
		tag = 0x0b
		dst = append(dst, v.Pattern...)
		dst = append(dst, 0)
		dst = append(dst, v.Options...)
		dst = append(dst, 0)
	case int32:
		tag = 0x10
		dst = binary.LittleEndian.AppendUint32(dst, uint32(v))
	case Timestamp:
		tag = 0x11
		dst = binary.LittleEndian.AppendUint64(dst, uint64(v))
	case int64:
		tag = 0x12
		dst = binary.LittleEndian.AppendUint64(dst, uint64(v))
	case Decimal128:
		tag = 0x13
		dst = binary.LittleEndian.AppendUint64(dst, v.L)
		dst = binary.LittleEndian.AppendUint64(dst, v.H)
	case MinKeyType:
		tag = 0xff
	case MaxKeyType:
		tag = 0x7f
	default:
		panic(fmt.Sprintf("invalid BSON value type %T", v))
	}
	dst[tagAt] = tag
	return dst
}
