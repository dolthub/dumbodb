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

package dolt

import (
	"context"
	"fmt"

	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
)

func (db *dbState) mergeBases(ctx context.Context, ours, theirs hash.Hash) ([]hash.Hash, error) {
	oursCommit, err := datas.LoadCommitAddr(ctx, db.vs, ours)
	if err != nil {
		return nil, err
	}
	theirsCommit, err := datas.LoadCommitAddr(ctx, db.vs, theirs)
	if err != nil {
		return nil, err
	}
	base, ok, err := datas.FindCommonAncestor(ctx, oursCommit, theirsCommit, db.vs, db.vs, db.ns, db.ns)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no common ancestor")
	}
	return []hash.Hash{base}, nil
}
