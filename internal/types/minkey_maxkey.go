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
	// MinKeyType represents the BSON MinKey type.
	// MinKey sorts before all other BSON types.
	//
	// Most callers should use types.MinKey value instead.
	MinKeyType struct{}

	// MaxKeyType represents the BSON MaxKey type.
	// MaxKey sorts after all other BSON types.
	//
	// Most callers should use types.MaxKey value instead.
	MaxKeyType struct{}
)

// MinKey is the singleton BSON MinKey value.
var MinKey = MinKeyType{}

// MaxKey is the singleton BSON MaxKey value.
var MaxKey = MaxKeyType{}

func (MinKeyType) LogValue() slog.Value {
	return slogValue(MinKeyType{}, 1)
}

func (MaxKeyType) LogValue() slog.Value {
	return slogValue(MaxKeyType{}, 1)
}

var (
	_ slog.LogValuer = MinKeyType{}
	_ slog.LogValuer = MaxKeyType{}
)
