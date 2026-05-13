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

// Package resource provides utilities for tracking resource lifetimes.
package resource

// Token should be a field of a tracked object.
//
// The underlying type is not struct{} because (from the Go spec)
// "Two distinct zero-size variables may have the same address in memory",
// and they do.
type Token byte

func NewToken() *Token {
	return new(Token)
}

// Track is a no-op retained for API compatibility.
func Track(obj any, token *Token) {}

// Untrack is a no-op retained for API compatibility.
func Untrack(obj any, token *Token) {}
