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

package bsonindexed

import (
	"crypto/sha512"
	"encoding/binary"
	"math"

	"github.com/zeebo/xxh3"
)

// Chunk-boundary constants and the weibull check are replicated from
// dolt's tree/node_splitter.go (which is package-private). The decision
// must match dolt byte-for-byte so the two implementations agree on
// where content-defined boundaries land.
const (
	MinChunkSize = 1 << 9
	MaxChunkSize = 1 << 14
	targetSize   = 1 << 12
	weibullL     = targetSize
)

var levelSaltZero = saltFromLevel(1)

const maxUint32 = float64(math.MaxUint32)

func CrossesBoundary(key []byte, totalSize uint32) bool {
	if totalSize < MinChunkSize {
		return false
	}
	if totalSize > MaxChunkSize {
		return true
	}
	h := xxHash32(key, levelSaltZero)
	return weibullCheck(totalSize, totalSize, h)
}

func weibullCheck(totalSize, thisSize, h uint32) bool {
	pow := float64(totalSize-thisSize) / weibullL
	start := -math.Expm1(-(pow * pow * pow * pow))
	pow = float64(totalSize) / weibullL
	end := -math.Expm1(-(pow * pow * pow * pow))
	p := float64(h) / maxUint32
	d := 1 - start
	if d <= 0 {
		return true
	}
	target := (end - start) / d
	return p < target
}

func xxHash32(b []byte, salt uint64) uint32 {
	return uint32(xxh3.HashSeed(b, salt))
}

// saltFromLevel: SHA-512 of {level}, first 8 bytes as little-endian uint64.
func saltFromLevel(level uint8) uint64 {
	full := sha512.Sum512([]byte{level})
	return binary.LittleEndian.Uint64(full[:8])
}
