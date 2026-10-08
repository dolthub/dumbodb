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

package common

import (
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// StageMemoryLimit is the memory one blocking aggregation stage may hold, as
// in MongoDB.
const StageMemoryLimit = 100 * 1024 * 1024

// StageBudget accounts the memory a blocking stage holds, measured as the BSON
// size of what it retains. DumboDB cannot spill to disk, so passing the limit
// fails the stage with QueryExceededMemoryLimitNoDiskUseAllowed, the error
// MongoDB returns when spilling is not allowed.
type StageBudget struct {
	stage string
	used  int
}

func NewStageBudget(stage string) *StageBudget {
	return &StageBudget{stage: stage}
}

func (b *StageBudget) Charge(n int) error {
	b.used += n
	if b.used <= StageMemoryLimit {
		return nil
	}
	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrQueryExceededMemoryLimitNoDiskUseAllowed,
		fmt.Sprintf("Exceeded memory limit for %s, but didn't allow external spilling; DumboDB cannot spill to disk", b.stage),
		b.stage,
	)
}

func (b *StageBudget) ChargeDocument(doc *types.Document) error {
	return b.Charge(doc.BSONSize())
}

// ConsumeDocuments drains iter like iterator.ConsumeValues, charging every
// document to a StageBudget for stage.
func ConsumeDocuments(iter types.DocumentsIterator, stage string) ([]*types.Document, error) {
	defer iter.Close()

	budget := NewStageBudget(stage)
	var docs []*types.Document
	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return docs, nil
		}
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
		if err = budget.ChargeDocument(doc); err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
}
