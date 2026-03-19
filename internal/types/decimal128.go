// Copyright 2021 FerretDB Inc.
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

// Decimal128 represents BSON scalar type decimal128.
//
// The value is stored as two uint64 fields (low and high) in little-endian
// IEEE 754 decimal128 encoding, matching the wire protocol representation.
type Decimal128 struct {
	L uint64
	H uint64
}
