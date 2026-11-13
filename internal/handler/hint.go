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

package handler

import (
	"context"
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// checkHintType returns mongod's FailedToParse for a hint that is neither a
// string nor a document, which mongod refuses before planning, as a command
// error even in a write statement.
func checkHintType(hint any) error {
	switch hint.(type) {
	case nil, string, *types.Document:
		return nil
	}
	return handlererrors.NewCommandErrorMsg(handlererrors.ErrFailedToParse, "Hint must be a string or an object")
}

// validateHintExists is validateHint for a read without a sort or upsert.
func validateHintExists(ctx context.Context, coll backends.Collection, hint any, command string) error {
	return validateHint(ctx, coll, hint, command, nil, false)
}

// validateHint returns mongod's error for a hint the command cannot use: a
// {$natural: n} with n other than 1 or -1, or in the opposite direction from a
// $natural sort; or a name or key pattern of no index on the collection. An
// empty name is no index's. A missing collection has no indexes, which mongod
// overlooks, even for an empty name, for a read but not for an upsert, which
// would create it.
func validateHint(ctx context.Context, coll backends.Collection, hint any, command string, sort *types.Document, upsert bool) error {
	if d, ok := hint.(*types.Document); ok && d.Len() > 1 && d.Has("$natural") {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
			"$natural hint cannot be set to a value other than -1 or 1.", command)
	}
	if d, ok := hint.(*types.Document); ok && backends.HintIsNatural(d) {
		dir, ok := naturalDirection(must.NotFail(d.Get("$natural")))
		if !ok {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
				"$natural hint cannot be set to a value other than -1 or 1.", command)
		}
		if sort.Len() == 1 && sort.Has("$natural") {
			if sortDir, ok := naturalDirection(must.NotFail(sort.Get("$natural"))); ok && sortDir != dir {
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
					"$natural hint must be in the same direction as $natural sort order", command)
			}
		}
		return nil
	}
	notIndexed := handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
		"hint provided does not correspond to an existing index", command)
	if hint != "" && !backends.HintRequiresExistingIndex(hint) {
		return nil
	}

	idxRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		// A read of a missing collection returns no documents, so mongod does
		// not validate its hint; an upsert would create the collection.
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
			backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
			if upsert {
				return notIndexed
			}
			return nil
		}
		return lazyerrors.Error(err)
	}

	if hint == "" || backends.MatchHintedIndex(hint, idxRes.Indexes) == "" {
		return notIndexed
	}

	return nil
}

// checkCompoundNaturalHint returns the error mongod gives a hint pairing
// $natural with index keys where it plans the read as a query: an aggregate
// led by $match, or a distinct.
func checkCompoundNaturalHint(hint any, command string) error {
	d, ok := hint.(*types.Document)
	if !ok || d.Len() < 2 || !d.Has("$natural") {
		return nil
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
		"Cannot include '$natural' in compound hint: "+formatDupKey(d), command)
}

// validateViewHint returns mongod's error for a hint on a read of a view,
// which mongod runs as an aggregate of the view's stages and the command's on
// the view's base collection, checking the hint there. commandLedByMatch
// reports whether the command's own stages start with a $match.
func validateViewHint(ctx context.Context, db backends.Database, view *backends.CollectionInfo, hint any, command string, commandLedByMatch bool) error {
	if hint == nil {
		return nil
	}
	if err := checkHintType(hint); err != nil {
		return err
	}
	base, _, rawStages, err := resolveViewChain(ctx, db, view.Name, view.ViewOn, view.ViewPipeline, nil)
	if err != nil {
		return nil
	}
	// find keeps its own rules for a hint, whatever the stages.
	ledByMatch := commandLedByMatch && len(rawStages) == 0
	if len(rawStages) > 0 {
		first, _ := rawStages[0].(*types.Document)
		ledByMatch = first != nil && first.Command() == "$match"
	}
	ledByMatch = ledByMatch && command != "find"
	if ledByMatch || command == "distinct" {
		if err = checkCompoundNaturalHint(hint, command); err != nil {
			return err
		}
	}
	if ledByMatch && backends.HintIsNatural(hint) {
		return nil
	}
	coll, err := db.Collection(base)
	if err != nil {
		return nil
	}
	return validateHintExists(ctx, coll, hint, command)
}

// collectionMissing reports whether coll does not exist.
func collectionMissing(ctx context.Context, coll backends.Collection) bool {
	_, err := coll.ListIndexes(ctx, nil)
	return backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
		backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist)
}

// naturalDirection returns 1 or -1 for a $natural hint or sort value: any
// number equal to one of them.
func naturalDirection(v any) (int, bool) {
	switch v.(type) {
	case int32, int64, float64, types.Decimal128:
	default:
		return 0, false
	}
	switch {
	case types.Compare(v, int32(1)) == types.Equal:
		return 1, true
	case types.Compare(v, int32(-1)) == types.Equal:
		return -1, true
	}
	return 0, false
}

// hintedIndexOrder returns the key pattern of the index hint names when a
// read hinted to it does not come back in that index's order on its own, else
// nil: when the index has a descending key, as DumboDB stores every key
// ascending, when filter's top level is an operator, which the backend
// does not answer by walking the hinted index, or when filter tests a key
// field with $type or $regex, which the backend does not bound, so it
// reaches an array by its least element rather than its first one in bounds. An unsorted hinted read returns
// index order, so such a read sorts by the pattern (SortIteratorInIndexOrder)
// and sortFilter, which is filter, or marks it read in one scan of the index.
// streamFilter, when set, is a filter a hinted backend read returns in index
// order without the sort (hintedStreamFilter).
// numToReturn is the results the query needs, which end a plan of its trial
// (TrialBudget), 0 for mongod's default.
func hintedIndexOrder(ctx context.Context, coll backends.Collection, hint any, filter, queryCollation *types.Document, numToReturn int) (order, sortFilter, streamFilter *types.Document, err error) {
	if !backends.HintRequiresExistingIndex(hint) {
		return nil, nil, nil, nil
	}
	idxRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		return nil, nil, nil, nil
	}
	name := backends.MatchHintedIndex(hint, idxRes.Indexes)
	for _, idx := range idxRes.Indexes {
		if idx.Name != name {
			continue
		}
		descending, unbounded := false, false
		order := types.MakeDocument(len(idx.Key))
		for _, k := range idx.Key {
			if k.Text || k.Geo2D || k.Geo2DSphere || k.Hashed {
				return nil, nil, nil, nil
			}
			dir := int32(1)
			if k.Descending {
				dir, descending = -1, true
			}
			order.Set(k.Field, dir)
			unbounded = unbounded || typeOrRegexCondition(filter, k.Field)
		}
		// The backend scans a sparse or partial index unbounded.
		unbounded = unbounded || idx.Sparse || idx.PartialFilterExpression != nil
		if descending || unbounded || topLevelOperator(filter) {
			if (idx.Sparse || idx.PartialFilterExpression != nil) && orBranchMatchesMissing(filter) {
				return order, common.AsOneIndexScan(filter), nil, nil
			}
			sortFilter = filter
			cmp := collation.Parse(queryCollation).Comparator()
			var budget *common.TrialBudget
			for i, k := range idx.Key {
				if !idx.KeyMultikey(i) {
					continue
				}
				if budget == nil {
					countRes, cerr := coll.Count(ctx, new(backends.CountParams))
					if cerr != nil {
						return nil, nil, nil, cerr
					}
					budget = common.NewTrialBudget(countRes.Count, numToReturn)
				}
				rangeIter := func(cond *types.Document) (types.DocumentsIterator, *types.Document, error) {
					return indexRangeIter(ctx, coll, idx, hint, k.Field, cond, queryCollation)
				}
				if sortFilter, err = common.BoundedByTrial(ctx, sortFilter, k.Field, rangeIter, budget, cmp); err != nil {
					return nil, nil, nil, err
				}
			}
			return order, sortFilter, hintedStreamFilter(idx, filter, sortFilter), nil
		}
	}
	return nil, nil, nil, nil
}

// hintedStreamFilter returns sortFilter when a hinted backend read of it
// returns filter's matches in index order: a walk of one ascending top-level
// key, in an index that is neither sparse nor partial, over the one bound of
// a multikey range the plan trial kept, reaches each document at its first key
// in that bound, as mongod's scan does. filter must test that field alone.
func hintedStreamFilter(idx backends.IndexInfo, filter, sortFilter *types.Document) *types.Document {
	if len(idx.Key) != 1 || idx.Key[0].Descending || strings.Contains(idx.Key[0].Field, ".") ||
		idx.Sparse || idx.PartialFilterExpression != nil || !idx.KeyMultikey(0) ||
		filter.Len() != 1 || filter.Keys()[0] != idx.Key[0].Field || sortFilter == filter {
		return nil
	}
	ops, ok := must.NotFail(sortFilter.Get(idx.Key[0].Field)).(*types.Document)
	if !ok || ops.Len() != 1 {
		return nil
	}
	switch ops.Keys()[0] {
	case "$gt", "$gte", "$lt", "$lte":
		return sortFilter
	}
	return nil
}

// hintedFirstMatches returns iter's matches of filter, a hinted single-document
// write's candidates, in the hinted index's order: by a fresh hinted read of
// the stream filter when there is one, else sorted.
func hintedFirstMatches(ctx context.Context, coll backends.Collection, hint any, filter, queryCollation *types.Document, iter types.DocumentsIterator, closer *iterator.MultiCloser, cmp *collation.Comparator) (types.DocumentsIterator, error) {
	order, sortFilter, streamFilter, err := hintedIndexOrder(ctx, coll, hint, filter, queryCollation, 0)
	switch {
	case err != nil:
		return nil, err
	case streamFilter != nil:
		res, err := coll.Query(ctx, &backends.QueryParams{
			Filter: streamFilter, Hint: hint, Collation: queryCollation, Collated: cmp != nil,
		})
		if err != nil {
			return nil, err
		}
		closer.Add(res.Iter)
		return common.FilterIteratorColl(iterator.WithContext(ctx, res.Iter), closer, filter, cmp), nil
	case order != nil:
		return common.SortIteratorInIndexOrder(iter, closer, order, sortFilter, cmp)
	}
	return iter, nil
}

// indexRangeIter returns, in the hinted index's order, the documents a
// hinted read of cond on field returns, and the filter of those idx holds
// with a key cond admits.
func indexRangeIter(ctx context.Context, coll backends.Collection, idx backends.IndexInfo, hint any, field string, cond, queryCollation *types.Document) (types.DocumentsIterator, *types.Document, error) {
	inRange := must.NotFail(types.NewDocument(field, cond))
	res, err := coll.Query(ctx, &backends.QueryParams{
		Filter: inRange, Hint: hint, Collation: queryCollation, Collated: !collation.Parse(queryCollation).IsSimple(),
	})
	if err != nil {
		return nil, nil, err
	}
	member := types.MakeArray(3)
	member.Append(inRange)
	if idx.PartialFilterExpression != nil {
		member.Append(idx.PartialFilterExpression)
	}
	if idx.Sparse {
		member.Append(must.NotFail(types.NewDocument(field, must.NotFail(types.NewDocument("$exists", true)))))
	}
	return iterator.WithContext(ctx, res.Iter), must.NotFail(types.NewDocument("$and", member)), nil
}

// orBranchMatchesMissing reports whether filter is a lone $or with a branch
// that matches a document without its fields, such as a negation or a null
// test: a sparse or partial index cannot bound that branch, so mongod reads
// the hinted index whole in one scan.
func orBranchMatchesMissing(filter *types.Document) bool {
	if filter.Len() != 1 || filter.Keys()[0] != "$or" {
		return false
	}
	branches, _ := must.NotFail(filter.Get("$or")).(*types.Array)
	if branches == nil {
		return false
	}
	empty := types.MakeDocument(0)
	for i := 0; i < branches.Len(); i++ {
		b, ok := must.NotFail(branches.Get(i)).(*types.Document)
		if !ok {
			continue
		}
		if matched, err := common.FilterDocument(empty, b); err == nil && matched {
			return true
		}
	}
	return false
}

// typeOrRegexCondition reports whether filter tests field with $type or
// $regex, matches it against a regular expression, compares it more than
// once, a bound a multikey walk takes from one comparison alone, or tests it
// for null or with an $in holding null or a regular expression, which the
// backend scans unbounded.
func typeOrRegexCondition(filter *types.Document, field string) bool {
	v, err := filter.Get(field)
	if err != nil {
		return false
	}
	switch v := v.(type) {
	case types.Regex, types.NullType:
		return true
	case *types.Document:
		ranges := 0
		for _, op := range []string{"$eq", "$lt", "$lte", "$gt", "$gte"} {
			if v.Has(op) {
				ranges++
			}
		}
		return v.Has("$type") || v.Has("$regex") || ranges > 1 || inHoldsNullOrRegex(v) || comparesNull(v)
	}
	return false
}

// comparesNull reports whether ops holds $eq, $lte or $gte of null, which the
// backend scans unbounded.
func comparesNull(ops *types.Document) bool {
	for _, op := range []string{"$eq", "$lte", "$gte"} {
		if v, err := ops.Get(op); err == nil && v == types.Null {
			return true
		}
	}
	return false
}

func inHoldsNullOrRegex(ops *types.Document) bool {
	in, err := ops.Get("$in")
	arr, ok := in.(*types.Array)
	if err != nil || !ok {
		return false
	}
	for i := 0; i < arr.Len(); i++ {
		switch must.NotFail(arr.Get(i)).(type) {
		case types.NullType, types.Regex:
			return true
		}
	}
	return false
}

// topLevelOperator reports whether filter's top level holds an operator such
// as $or, $and, $nor or $expr.
func topLevelOperator(filter *types.Document) bool {
	for _, k := range filter.Keys() {
		if strings.HasPrefix(k, "$") && k != "$comment" {
			return true
		}
	}
	return false
}

// clusteredHint maps a hint naming a collection's custom clustered index,
// recorded as clusteredName, to the _id index that stores it, and the default
// name _id_, which then names no index, to a name no index has.
func clusteredHint(hint any, clusteredName string) any {
	if clusteredName == "" || clusteredName == backends.DefaultIndexName {
		return hint
	}
	switch hint {
	case clusteredName:
		return backends.DefaultIndexName
	case backends.DefaultIndexName:
		return "\x00" + backends.DefaultIndexName
	}
	return hint
}

// recordedClusteredName returns the clustered index name the collection was
// created with, or "".
func recordedClusteredName(ctx context.Context, db backends.Database, collection string) string {
	if info, err := lookupCollectionInfo(ctx, db, collection); err == nil && info != nil {
		return info.ClusteredIndexName
	}
	return ""
}
