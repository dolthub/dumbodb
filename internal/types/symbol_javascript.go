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

// Symbol is the deprecated BSON Symbol type (0x0E). It sorts and compares
// equal to a string with the same content, but keeps its own BSON type.
type Symbol string

// JavaScript is the BSON JavaScript code type (0x0D), held as a value; it is
// never executed.
type JavaScript string
