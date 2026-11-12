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
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
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
// index order, so such a read sorts by the pattern (SortIteratorInIndexOrder).
func hintedIndexOrder(ctx context.Context, coll backends.Collection, hint any, filter *types.Document) (*types.Document, error) {
	if !backends.HintRequiresExistingIndex(hint) {
		return nil, nil
	}
	idxRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		return nil, nil
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
				return nil, nil
			}
			dir := int32(1)
			if k.Descending {
				dir, descending = -1, true
			}
			order.Set(k.Field, dir)
			unbounded = unbounded || typeOrRegexCondition(filter, k.Field)
		}
		if descending || unbounded || topLevelOperator(filter) {
			return order, nil
		}
	}
	return nil, nil
}

// typeOrRegexCondition reports whether filter tests field with $type or
// $regex, matches it against a regular expression, or compares it more than
// once: bounds a multikey walk takes from one comparison alone.
func typeOrRegexCondition(filter *types.Document, field string) bool {
	v, err := filter.Get(field)
	if err != nil {
		return false
	}
	switch v := v.(type) {
	case types.Regex:
		return true
	case *types.Document:
		ranges := 0
		for _, op := range []string{"$eq", "$lt", "$lte", "$gt", "$gte"} {
			if v.Has(op) {
				ranges++
			}
		}
		return v.Has("$type") || v.Has("$regex") || ranges > 1
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
