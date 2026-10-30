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

package iterator

import "context"

type withContextIterator[K, V any] struct {
	ctx  context.Context
	iter Interface[K, V]
}

// WithContext wraps an iterator so that Next returns ctx.Err() once ctx is
// done, letting work done per item (such as expression evaluation in later
// stages) stop at a deadline instead of running to the end.
func WithContext[K, V any](ctx context.Context, iter Interface[K, V]) Interface[K, V] {
	return &withContextIterator[K, V]{ctx: ctx, iter: iter}
}

func (iter *withContextIterator[K, V]) Next() (K, V, error) {
	if err := iter.ctx.Err(); err != nil {
		var k K
		var v V
		return k, v, err
	}
	return iter.iter.Next()
}

func (iter *withContextIterator[K, V]) Close() {
	iter.iter.Close()
}

var _ Interface[any, any] = (*withContextIterator[any, any])(nil)
