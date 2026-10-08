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

//go:build !windows

package clientconn

import "syscall"

// DefaultMaxConns follows mongod: 1,000,000 connections, but at most 80% of
// the process's open-file limit.
func DefaultMaxConns() int {
	const mongoDefault = 1_000_000
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil || limit.Cur == 0 {
		return mongoDefault
	}
	return int(min(uint64(mongoDefault), max(uint64(limit.Cur)*8/10, 1)))
}
