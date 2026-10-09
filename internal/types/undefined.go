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

import "log/slog"

type (
	// UndefinedType represents the deprecated BSON Undefined type. It is never
	// accepted from clients; the server emits it only where MongoDB does, such
	// as the index key of an empty array returned by distinct.
	//
	// Most callers should use types.Undefined value instead.
	UndefinedType struct{}
)

var Undefined = UndefinedType{}

func (UndefinedType) LogValue() slog.Value {
	return slogValue(UndefinedType{}, 1)
}

var _ slog.LogValuer = UndefinedType{}
