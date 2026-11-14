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

package stages

import (
	"context"
	"slices"
	"strings"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// match represents $match stage.
type match struct {
	filter *types.Document
	cmp    *collation.Comparator
	// plan is the order mongod checks the filter's conditions in: that of
	// the query it is pushed into, or, by default, $expr first.
	plan common.PlanMode
}

func newMatch(stage *types.Document) (aggregations.Stage, error) {
	return NewMatchStage(stage, nil, true)
}

// NewMatchStage builds a $match stage whose string comparisons honor cmp when
// non-nil. storedInput reports whether the stage sees documents as stored
// (see SeesStoredDocuments); only then are the document values it compares
// against put in stored key order.
func NewMatchStage(stage *types.Document, cmp *collation.Comparator, storedInput bool) (aggregations.Stage, error) {
	if cmp == nil {
		cmp = operators.CollationOf(stage)
	}
	filter, err := common.GetRequiredParam[*types.Document](stage, "$match")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrMatchBadExpression,
			"the match filter must be an expression in an object",
			"$match (stage)",
		)
	}

	// Canonicalize first: validation builds the filter's operators, which
	// must see the values they will compare.
	if storedInput {
		common.CanonicalizeFilter(filter)
	}

	if err := validateMatch(filter); err != nil {
		return nil, err
	}
	// mongod parses the whole pipeline before running it, so a malformed
	// filter fails before any later stage's errors.
	if err := common.ValidateFilter(filter, cmp); err != nil {
		return nil, err
	}

	return &match{filter: filter, cmp: cmp, plan: common.PlanUnpushed}, nil
}

// newValidatedMatchStage is NewMatchStage for a $match stage whose filter
// has already passed validation.
func newValidatedMatchStage(stage *types.Document, storedInput bool) aggregations.Stage {
	filter := must.NotFail(stage.Get("$match")).(*types.Document)
	if storedInput {
		common.CanonicalizeFilter(filter)
	}
	return &match{filter: filter, cmp: operators.CollationOf(stage), plan: common.PlanUnpushed}
}

func (m *match) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	return common.FilterIteratorPlan(iter, closer, m.filter, m.cmp, m.plan), nil
}

// SetPushedMatchPlan gives a $match that mongod pushes into the query layer
// the plan mode of the query answering it, and reports whether stage is a
// $match.
func SetPushedMatchPlan(stage aggregations.Stage, mode common.PlanMode) bool {
	if s, ok := stage.(*sbeStage); ok {
		stage = s.inner
	}
	m, ok := stage.(*match)
	if ok {
		m.plan = mode
	}
	return ok
}

// validateMatch validates $expr field if any.
func validateMatch(filter *types.Document) error {
	if filter.Has("$expr") {
		_, err := operators.NewExpr(filter, "$match (stage)")
		if err != nil {
			return err
		}
	}

	// $near / $nearSphere require a sort context, so they are not permitted in
	// a $match stage (the path used by count and general aggregation). MongoDB
	// rejects them here with Location5626500.
	if common.FindGeoSortKey(filter) != nil {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrGeoNearNotAllowedInContext,
			common.GeoNearNotAllowedMsg,
		)
	}

	return nil
}

var (
	_ aggregations.Stage = (*match)(nil)
)

// SeesStoredDocuments reports whether stage i of pipeline receives documents
// exactly as stored: every stage before it passes its input on unchanged.
// Stored documents have sorted keys; documents a stage builds keep the order
// they were built in.
func SeesStoredDocuments(pipeline []any, i int) bool {
	for _, stage := range pipeline[:i] {
		doc, ok := stage.(*types.Document)
		if !ok || doc.Len() == 0 {
			return false
		}
		switch doc.Command() {
		case "$match", "$sort", "$limit", "$skip", "$sample":
		default:
			return false
		}
	}
	return true
}

// KeepsRecordID reports whether documents leaving s keep their RecordId: only
// $match passes it on, as in mongod.
func KeepsRecordID(s aggregations.Stage) bool {
	_, ok := s.(*match)
	return ok
}

// PushedMatches returns the $match stages of pipeline mongod pushes into the
// query that reads its input: the leading ones, also those after a leading
// $sort, which mongod moves ahead of it. filter is their conjunction, nil
// when there are none.
func PushedMatches(pipeline []aggregations.Stage) (matches []aggregations.Stage, filter *types.Document) {
	clauses := types.MakeArray(0)
stages:
	for _, stage := range pipeline {
		inner := stage
		if s, ok := inner.(*sbeStage); ok {
			inner = s.inner
		}
		switch inner := inner.(type) {
		case *sort:
		case *match:
			matches = append(matches, stage)
			clauses.Append(inner.filter)
		default:
			break stages
		}
	}
	switch clauses.Len() {
	case 0:
		return nil, nil
	case 1:
		return matches, must.NotFail(clauses.Get(0)).(*types.Document)
	}
	return matches, must.NotFail(types.NewDocument("$and", clauses))
}

// filterFields returns the fields filter has conditions on, at the top level
// or in logical operators' clauses.
func filterFields(filter *types.Document, fields []string) []string {
	keys, values := filter.Keys(), filter.Values()
	for i, key := range keys {
		switch key {
		case "$and", "$or", "$nor":
			clauses, _ := values[i].(*types.Array)
			for j := 0; clauses != nil && j < clauses.Len(); j++ {
				if clause, ok := must.NotFail(clauses.Get(j)).(*types.Document); ok {
					fields = filterFields(clause, fields)
				}
			}
		default:
			if !strings.HasPrefix(key, "$") && !slices.Contains(fields, key) {
				fields = append(fields, key)
			}
		}
	}
	return fields
}
