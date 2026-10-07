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

// Package identitycache memoizes values built from a document, such as a query
// filter, that is evaluated many times, keyed by the document's identity.
package identitycache

import (
	"runtime"
	"sync"
	"unsafe"
	"weak"
)

// Cache maps *K to V. Entries are removed when their key is garbage collected.
type Cache[K any, V any] struct {
	mu sync.RWMutex
	m  map[uintptr]*entry[K, V]
}

type entry[K any, V any] struct {
	key weak.Pointer[K]
	v   V
}

func (c *Cache[K, V]) Get(k *K) (V, bool) {
	c.mu.RLock()
	e, ok := c.m[uintptr(unsafe.Pointer(k))]
	c.mu.RUnlock()
	if ok && e.key.Value() == k {
		return e.v, true
	}
	var zero V
	return zero, false
}

func (c *Cache[K, V]) Put(k *K, v V) {
	addr := uintptr(unsafe.Pointer(k))
	e := &entry[K, V]{key: weak.Make(k), v: v}
	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[uintptr]*entry[K, V])
	}
	c.m[addr] = e
	c.mu.Unlock()
	runtime.AddCleanup(k, c.remove, cleanupArg[K, V]{addr: addr, e: e})
}

type cleanupArg[K any, V any] struct {
	addr uintptr
	e    *entry[K, V]
}

func (c *Cache[K, V]) remove(a cleanupArg[K, V]) {
	c.mu.Lock()
	if c.m[a.addr] == a.e {
		delete(c.m, a.addr)
	}
	c.mu.Unlock()
}

func (c *Cache[K, V]) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}
