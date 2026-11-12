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

package handler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgCreateIndexes(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document,
		"indexes",
		"commitQuorum",
		"ignoreUnknownIndexOptions",
	); err != nil {
		return nil, err
	}

	command := document.Command()

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	if err = enforceWritableRootish(dbName); err != nil {
		return nil, err
	}

	collection, err := common.GetCollectionNameParam(document, command)
	if err != nil {
		return nil, err
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				fmt.Sprintf("Invalid namespace specified '%s.%s'", dbName, collection),
				command,
			)
		}

		return nil, lazyerrors.Error(err)
	}

	c, err := db.Collection(collection)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				fmt.Sprintf("Invalid namespace specified '%s.%s'", dbName, collection),
				command,
			)
		}

		return nil, lazyerrors.Error(err)
	}

	v, _ := document.Get("indexes")
	if v == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrMissingField,
			"BSON field 'createIndexes.indexes' is missing but a required field",
			document.Command(),
		)
	}

	idxArr, ok := v.(*types.Array)
	if !ok {
		if _, ok = v.(types.NullType); ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrExpectedObject,
				"invalid parameter: expected an object (indexes)",
				document.Command(),
			)
		}

		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf(
				"BSON field 'createIndexes.indexes' is the wrong type '%s', expected type 'array'",
				handlerparams.AliasFromType(v),
			),
			document.Command(),
		)
	}

	if idxArr.Len() == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"Must specify at least one index to create",
			document.Command(),
		)
	}

	ignoreUnknownIndexOptions, err := common.GetOptionalBoolOrIntParam(document, "ignoreUnknownIndexOptions", false)
	if err != nil {
		return nil, err
	}

	toCreate, err := processIndexesArray(command, idxArr, ignoreUnknownIndexOptions)
	if err != nil {
		return nil, err
	}

	// An index created without an explicit collation inherits the collection's
	// default collation.
	var collectionCollation *types.Document
	cInfo, cerr := lookupCollectionInfo(connCtx, db, collection)
	if cerr != nil {
		cInfo = nil
	}
	if cInfo != nil {
		if err = checkClusteredIndexName(command, cInfo.ClusteredIndexName, toCreate); err != nil {
			return nil, err
		}
		if cInfo.Collation != nil {
			collectionCollation = cInfo.Collation
			for i := range toCreate {
				if toCreate[i].Collation == nil {
					toCreate[i].Collation = cInfo.Collation
				}
			}
		}
	}

	for _, idx := range toCreate {
		if formatIndexKey(idx.Key) == "_id: 1" && !sameCollation(idx.Collation, collectionCollation) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf(
					"The _id index must have the same collation as the collection. Index collation: %s, collection collation: %s",
					resolvedCollationString(idx.Collation), resolvedCollationString(collectionCollation),
				),
				command,
			)
		}
	}

	var createCollection bool
	beforeCreate, err := c.ListIndexes(connCtx, new(backends.ListIndexesParams))
	if err != nil {
		switch {
		case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist):
			// If the namespace doesn't exist, we just don't need to compare new indexes with existing ones,
			// the namespace will be created when indexes are created.
			beforeCreate = &backends.ListIndexesResult{
				Indexes: []backends.IndexInfo{},
			}
			createCollection = true

		default:
			return nil, lazyerrors.Error(err)
		}
	}

	visibleBefore := clientVisibleIndexes(connCtx, db, collection, beforeCreate.Indexes)
	numIndexesBefore := len(visibleBefore)
	if len(visibleBefore) != len(beforeCreate.Indexes) {
		// A time-series collection: mongod builds its indexes on the buckets,
		// where _id is control.min._id and control.max._id.
		for _, idx := range toCreate {
			if idx.Name == backends.DefaultIndexName {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
					"The index name '_id_' is reserved for the _id index, which must have key pattern {_id: 1}, "+
						"found key: { control.min._id: 1, control.max._id: 1 }", command)
			}
		}
	}

	// A collection created here counts its _id index as already present.
	if createCollection {
		numIndexesBefore = 1
	}

	// A clustered collection's clustered index is not counted, in the reply
	// or toward the limit.
	if !createCollection && cInfo != nil && cInfo.ClusteredIndexName != "" {
		numIndexesBefore--
	}

	// An existing collection with room for an index starts a build, which
	// fails naming the collection's UUID.
	buildingIn := ""
	if !createCollection && cInfo != nil && numIndexesBefore < maxIndexes {
		buildingIn = cInfo.UUID
	}

	requested := len(toCreate)
	// A request that cannot fit, whatever already exists, is refused before
	// its specs are compared pairwise.
	if len(toCreate) > maxIndexes {
		existingNames := make(map[string]struct{}, len(beforeCreate.Indexes))
		for _, idx := range beforeCreate.Indexes {
			existingNames[idx.Name] = struct{}{}
		}
		fresh := make([]backends.IndexInfo, 0, len(toCreate))
		for _, idx := range toCreate {
			if _, ok := existingNames[idx.Name]; !ok {
				fresh = append(fresh, idx)
			}
		}
		if err = checkIndexCount(command, dbName, collection, numIndexesBefore, fresh, buildingIn); err != nil {
			return nil, err
		}
	}
	timeSeries := isTimeSeriesCollection(connCtx, db, collection)
	if toCreate, err = validateIndexesForCreation(command, visibleBefore, toCreate, timeSeries); err != nil {
		return nil, err
	}
	if err = checkIndexCount(command, dbName, collection, numIndexesBefore, toCreate, buildingIn); err != nil {
		return nil, err
	}

	_, err = c.CreateIndexes(connCtx, &backends.CreateIndexesParams{Indexes: toCreate})
	if err != nil {
		// Building a unique index over data that already contains duplicate
		// values fails like a duplicate-key write (E11000), matching MongoDB.
		if backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrDuplicateKeyInsert,
				fmt.Sprintf("E11000 duplicate key error collection: %s.%s", dbName, collection),
			)
		}
		return nil, common.TranslateBackendWriteError(connCtx, err)
	}

	resp := new(types.Document)

	resp.Set("numIndexesBefore", int32(numIndexesBefore))
	resp.Set("numIndexesAfter", int32(numIndexesBefore+len(toCreate)))

	switch {
	case len(toCreate) == 0:
		resp.Set("note", "all indexes already exist")
	case len(toCreate) < requested:
		resp.Set("createdCollectionAutomatically", createCollection)
		resp.Set("note", "index already exists")
	default:
		resp.Set("createdCollectionAutomatically", createCollection)
	}

	resp.Set("ok", float64(1))

	return documentOpMsg(
		resp,
	)
}

func processIndexesArray(command string, indexesArray *types.Array, ignoreUnknownIndexOptions bool) ([]backends.IndexInfo, error) {
	iter := indexesArray.Iterator()
	defer iter.Close()

	res := make([]backends.IndexInfo, indexesArray.Len())

	for {
		var key, val any
		key, val, err := iter.Next()

		switch {
		case err == nil:
		case errors.Is(err, iterator.ErrIteratorDone):
			return res, nil
		default:
			return nil, lazyerrors.Error(err)
		}

		indexDoc, ok := val.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf(
					"BSON field 'createIndexes.indexes.%d' is the wrong type '%s', expected type 'object'",
					key,
					handlerparams.AliasFromType(val),
				),
				command,
			)
		}

		indexInfo, err := processIndex(command, indexDoc, ignoreUnknownIndexOptions)
		if err != nil {
			return nil, err
		}

		res[key.(int)] = *indexInfo
	}
}

func processIndex(command string, indexDoc *types.Document, ignoreUnknownIndexOptions bool) (*backends.IndexInfo, error) {
	var index backends.IndexInfo

	iter := indexDoc.Iterator()
	defer iter.Close()

	var hasValue bool

	for {
		opt, _, err := iter.Next()

		switch {
		case err == nil:
		case errors.Is(err, iterator.ErrIteratorDone):
			if !hasValue {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrFailedToParse,
					fmt.Sprintf(
						"Error in specification {} :: caused by :: "+
							"The 'key' field is a required property of an index specification",
					),
					command,
				)
			}

			// Hashed indexes cannot have a unique constraint.
			if index.Unique {
				for _, kp := range index.Key {
					if kp.Hashed {
						return nil, handlererrors.NewCommandErrorMsgWithArgument(
							handlererrors.ErrCannotCreateIndex,
							"Hashed indexes cannot guarantee uniqueness. Please remove the unique option and rerun.",
							command,
						)
					}
				}
			}

			if index.PartialFilterExpression != nil {
				index.MatchesPartialFilter = backends.PartialFilterMembership(index.PartialFilterExpression, index.Collation)
			}

			return &index, nil
		default:
			return nil, lazyerrors.Error(err)
		}

		hasValue = true

		var keyDoc *types.Document

		keyDoc, err = common.GetRequiredParam[*types.Document](indexDoc, "key")
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"'key' option must be specified as an object",
				"createIndexes",
			)
		}

		if keyDoc.Len() == 0 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrCannotCreateIndex,
				"Must specify at least one field for the index key",
				"createIndexes",
			)
		}

		// Special case: if keyDocs consists of a {"_id": -1} only, an error should be returned.
		if keyDoc.Len() == 1 {
			var val any
			var order int64

			if val, err = keyDoc.Get("_id"); err == nil {
				if order, err = handlerparams.GetWholeNumberParam(val); err == nil && order == -1 {
					return nil, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						"The field 'key' for an _id index must be {_id: 1}, but got { _id: -1 }",
						"createIndexes",
					)
				}
			}
		}

		index.Key, err = processIndexKey(command, keyDoc)
		if err != nil {
			return nil, err
		}

		v, _ := indexDoc.Get("name")
		if v == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf(
					"Error in specification { key: %s } :: caused by :: "+
						"The 'name' field is a required property of an index specification",
					types.FormatAnyValue(keyDoc),
				),
				"createIndexes",
			)
		}

		var ok bool
		index.Name, ok = v.(string)

		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"'name' option must be specified as a string",
				"createIndexes",
			)
		}

		switch opt {
		case "key", "name":

		case "unique":
			v := must.NotFail(indexDoc.Get("unique"))

			unique, ok := v.(bool)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch,
					fmt.Sprintf(
						"Error in specification { key: %s, name: %q, unique: %s } "+
							":: caused by :: "+
							"The field 'unique has value unique: %[3]s, which is not convertible to bool",
						types.FormatAnyValue(must.NotFail(indexDoc.Get("key"))),
						index.Name, types.FormatAnyValue(v),
					),
					command,
				)
			}

			if len(index.Key) == 1 && index.Key[0].Field == "_id" {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrInvalidIndexSpecificationOption,
					fmt.Sprintf("The field 'unique' is not valid for an _id index specification. "+
						"Specification: { key: %s, name: %q, unique: true, v: 2 }",
						types.FormatAnyValue(must.NotFail(indexDoc.Get("key"))), index.Name,
					),
					command,
				)
			}

			if unique {
				index.Unique = true
			}

		case "background":
			// ignore deprecated options

		case "sparse":
			if err := idIndexOptionError(command, index, indexDoc, "sparse"); err != nil {
				return nil, err
			}
			v := must.NotFail(indexDoc.Get("sparse"))
			if sparse, ok := v.(bool); ok && sparse {
				index.Sparse = true
			}

		case "weights", "default_language", "language_override", "textIndexVersion",
			"2dsphereIndexVersion":
			// Index options  -- accepted but not stored in the backend for now.

		case "partialFilterExpression":
			if err := idIndexOptionError(command, index, indexDoc, "partialFilterExpression"); err != nil {
				return nil, err
			}
			// Store the partial filter expression so that unique enforcement can
			// exclude documents that don't satisfy the filter.
			if pfe, ok := must.NotFail(indexDoc.Get("partialFilterExpression")).(*types.Document); ok {
				index.PartialFilterExpression = pfe
			}

		case "expireAfterSeconds":
			// TTL indexes are rejected rather than silently accepted: a
			// wall-clock sweeper that deletes data conflicts with version
			// control. Matches the createCollection rejection.
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidOptions,
				"TTL indexes (expireAfterSeconds) are not supported by DumboDB",
				command,
			)

		case "collation":
			// Stored and echoed by listIndexes. Not yet used for index
			// identity or runtime enforcement; that is deferred to the
			// collation engine work, which resolves the spec first.
			if coll, ok := must.NotFail(indexDoc.Get("collation")).(*types.Document); ok {
				index.Collation = coll
			}

		case "hidden":
			// Tracked and echoed by listIndexes; planner-level hiding is not
			// yet applied.
			if hidden, ok := must.NotFail(indexDoc.Get("hidden")).(bool); ok {
				index.Hidden = hidden
			}

		case "storageEngine",
			"bits", "min", "max", "bucketSize", "wildcardProjection":
			// Accepted but not enforced  -- stored index behaves as a regular index.

		default:
			if ignoreUnknownIndexOptions {
				continue
			}

			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidIndexSpecificationOption,
				fmt.Sprintf("The field '%s' is not valid for an index specification", opt),
				command,
			)
		}
	}
}

func processIndexKey(command string, keyDoc *types.Document) ([]backends.IndexKeyPair, error) {
	res := make([]backends.IndexKeyPair, 0, keyDoc.Len())

	keyIter := keyDoc.Iterator()
	defer keyIter.Close()

	duplicateChecker := make(map[string]struct{}, keyDoc.Len())

	for {
		field, order, err := keyIter.Next()

		switch {
		case err == nil:
		case errors.Is(err, iterator.ErrIteratorDone):
			return res, nil
		default:
			return nil, lazyerrors.Error(err)
		}

		if _, ok := duplicateChecker[field]; ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf(
					"Error in specification %s, the field %q appears multiple times",
					types.FormatAnyValue(keyDoc), field,
				),
				command,
			)
		}

		duplicateChecker[field] = struct{}{}

		if orderStr, ok := order.(string); ok {
			switch orderStr {
			case "text":
				res = append(res, backends.IndexKeyPair{
					Field: field,
					Text:  true,
				})
			case "2dsphere":
				res = append(res, backends.IndexKeyPair{
					Field:       field,
					Geo2DSphere: true,
				})
			case "2d":
				res = append(res, backends.IndexKeyPair{
					Field: field,
					Geo2D: true,
				})
			case "hashed":
				res = append(res, backends.IndexKeyPair{
					Field:  field,
					Hashed: true,
				})
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrIndexNotFound,
					fmt.Sprintf("can't find index with key: { %s: %q }", field, order),
					command,
				)
			}
			continue
		}

		var orderParam int64

		if orderParam, err = handlerparams.GetWholeNumberParam(order); err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrIndexNotFound,
				fmt.Sprintf("can't find index with key: { %s: %q }", field, order),
				command,
			)
		}

		var descending bool

		switch orderParam {
		case 1:
			descending = false
		case -1:
			descending = true
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNotImplemented,
				fmt.Sprintf("Index key value %d is not implemented yet", orderParam),
				command,
			)
		}

		res = append(res, backends.IndexKeyPair{
			Field:      field,
			Descending: descending,
		})
	}
}

func formatIndexKey(key []backends.IndexKeyPair) string {
	res := make([]string, len(key))

	for i, pair := range key {
		var order string
		switch {
		case pair.Text:
			order = `"text"`
		case pair.Geo2DSphere:
			order = `"2dsphere"`
		case pair.Geo2D:
			order = `"2d"`
		case pair.Hashed:
			order = `"hashed"`
		case pair.Descending:
			order = "-1"
		default:
			order = "1"
		}

		res[i] = pair.Field + ": " + order
	}

	return strings.Join(res, ", ")
}

// sameCollation reports whether two index collation specs denote the same
// collation, comparing normalized specs (nil and {locale:"simple"} both mean
// the binary default).
func resolvedCollationString(spec *types.Document) string {
	resolved := collation.Parse(spec).Resolve()
	if resolved == nil {
		return `{ locale: "simple" }`
	}
	return mongoCommandString(resolved)
}

func sameCollation(a, b *types.Document) bool {
	return collation.Parse(a).Identity() == collation.Parse(b).Identity()
}

// indexConflict returns mongod's error for creating idx beside other, or
// whether the two are identical, so idx need not be created. Indexes on one
// key differ when their unique, sparse, partialFilterExpression or collation
// do; other options, such as hidden, do not make them different indexes.
func indexConflict(command string, idx, other backends.IndexInfo, sameRequest bool) (bool, error) {
	if idx.Name != other.Name && !slices.EqualFunc(idx.Key, other.Key, func(a, b backends.IndexKeyPair) bool { return a == b }) {
		return false, nil
	}
	sameKey := formatIndexKey(idx.Key) == formatIndexKey(other.Key)
	equivalent := sameKey && idx.Unique == other.Unique && idx.Sparse == other.Sparse &&
		sameBSON(idx.PartialFilterExpression, other.PartialFilterExpression) && sameCollation(idx.Collation, other.Collation)
	sameOptions := equivalent && idx.Hidden == other.Hidden
	specs := func() string {
		return fmt.Sprintf("Requested index: %s, existing index: %s", conflictSpec(idx), conflictSpec(other))
	}
	switch {
	case idx.Name == other.Name && sameOptions:
		return true, nil
	case idx.Name == other.Name && equivalent:
		return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOptionsConflict,
			"An equivalent index already exists with the same name but different options. "+specs(), command)
	case idx.Name == other.Name:
		return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexKeySpecsConflict,
			"An existing index has the same name as the requested index."+
				" When index names are not specified, they are auto generated and can cause conflicts."+
				" Please refer to our documentation. "+specs(), command)
	case sameOptions || equivalent && sameRequest:
		return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOptionsConflict,
			"Index already exists with a different name: "+other.Name, command)
	case equivalent:
		return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOptionsConflict,
			"An equivalent index already exists with a different name and options. "+specs(), command)
	}
	return false, nil
}

// maxIndexes is the most indexes a collection may hold, its _id index
// included, as on mongod.
const maxIndexes = 64

// checkIndexCount returns mongod's error when creating toCreate beside
// existing indexes would pass maxIndexes, naming the first index past it.
// A build started in the collection with UUID buildingIn, when not empty,
// reports it as a failed build.
func checkIndexCount(command, dbName, collection string, existing int, toCreate []backends.IndexInfo, buildingIn string) error {
	if existing+len(toCreate) <= maxIndexes {
		return nil
	}
	first := toCreate[max(maxIndexes-existing, 0)]
	msg := fmt.Sprintf("add index fails, too many indexes for %s.%s key:{ %s }", dbName, collection, formatIndexKey(first.Key))
	if buildingIn != "" {
		msg = fmt.Sprintf("Index build failed: %s: Collection %s.%s ( %s ) :: caused by :: %s", uuid.NewString(), dbName, collection, buildingIn, msg)
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCannotCreateIndex, msg, command)
}

// conflictSpec is idx's spec as mongod prints it in an index conflict.
func conflictSpec(idx backends.IndexInfo) string {
	var b strings.Builder
	b.WriteString("{ v: 2")
	if idx.Hidden {
		b.WriteString(", hidden: true")
	}
	if idx.Unique {
		b.WriteString(", unique: true")
	}
	fmt.Fprintf(&b, ", key: { %s }, name: %q", formatIndexKey(idx.Key), idx.Name)
	if idx.Sparse {
		b.WriteString(", sparse: true")
	}
	if idx.PartialFilterExpression != nil {
		b.WriteString(", partialFilterExpression: " + types.FormatAnyValue(idx.PartialFilterExpression))
	}
	if idx.Collation != nil {
		b.WriteString(", collation: " + resolvedCollationString(idx.Collation))
	}
	b.WriteString(" }")
	return b.String()
}

// idIndexOptionError returns mongod's error for option, which an _id index
// spec may not hold, in indexDoc when index is on _id alone.
func idIndexOptionError(command string, index backends.IndexInfo, indexDoc *types.Document, option string) error {
	if len(index.Key) != 1 || index.Key[0].Field != "_id" {
		return nil
	}
	spec := strings.TrimSuffix(types.FormatAnyValue(indexDoc), " }") + ", v: 2 }"
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidIndexSpecificationOption,
		fmt.Sprintf("The field '%s' is not valid for an _id index specification. Specification: %s", option, spec), command)
}

// indexSpecString is idx's spec as mongod prints it in an error: its key,
// name and options, then v and a collation.
func indexSpecString(idx backends.IndexInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{ key: { %s }, name: %q", formatIndexKey(idx.Key), idx.Name)
	if idx.Unique {
		b.WriteString(", unique: true")
	}
	if idx.Sparse {
		b.WriteString(", sparse: true")
	}
	if idx.PartialFilterExpression != nil {
		b.WriteString(", partialFilterExpression: " + types.FormatAnyValue(idx.PartialFilterExpression))
	}
	if idx.Hidden {
		b.WriteString(", hidden: true")
	}
	b.WriteString(", v: 2")
	if idx.Collation != nil {
		b.WriteString(", collation: " + resolvedCollationString(idx.Collation))
	}
	b.WriteString(" }")
	return b.String()
}

// checkClusteredIndexName returns mongod's error for an index named like the
// collection's clustered index, other than one on _id, which is that index.
func checkClusteredIndexName(command, clusteredName string, toCreate []backends.IndexInfo) error {
	if clusteredName == "" || clusteredName == backends.DefaultIndexName {
		return nil
	}
	for _, idx := range toCreate {
		key := formatIndexKey(idx.Key)
		if idx.Name != clusteredName || key == "_id: 1" {
			continue
		}
		spec := indexSpecString(idx)
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexNameMatchesClusteredIndex, fmt.Sprintf(
			"Error in specification %s :: caused by :: Cannot create an index where the name matches the clusteredIndex but the key "+
				"does not - indexSpec: %s, clusteredIndex: { v: 2, key: { _id: 1 }, name: %q, unique: true }",
			spec, spec, clusteredName), command)
	}
	return nil
}

// validateIndexesForCreation filters duplicates out of toCreate and returns an
// error if any index conflicts with an existing one or has an invalid spec.
// A time-series collection has no _id index of its own, so an {_id: 1} index
// is created like any other.
func validateIndexesForCreation(command string, existing, toCreate []backends.IndexInfo, timeSeries bool) ([]backends.IndexInfo, error) {
	filteredToCreate := make([]backends.IndexInfo, 0, len(toCreate))

	for i, newIdx := range toCreate {
		newKey := formatIndexKey(newIdx.Key)

		if newIdx.Name == "" {
			msg := fmt.Sprintf(
				"Error in specification { key: { %s }, name: %q, v: 2 } :: caused by :: index name cannot be empty",
				newKey, newIdx.Name,
			)

			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCannotCreateIndex, msg, command)
		}

		if strings.Contains(newIdx.Name, "\x00") {
			msg := fmt.Sprintf(
				"Error in specification { key: { %s }, name: \"%s\", v: 2 } :: caused by :: index name cannot contain NUL bytes",
				newKey, newIdx.Name,
			)

			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCannotCreateIndex, msg, command)
		}

		// "*" means every index to dropIndexes.
		if newIdx.Name == "*" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, "The index name '*' is not valid.", command)
		}

		if newIdx.Name == backends.DefaultIndexName && newKey != "_id: 1" {
			msg := fmt.Sprintf(
				"The index name '_id_' is reserved for the _id index, which must have key pattern {_id: 1},"+
					" found key: { %s }",
				newKey,
			)

			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, msg, command)
		}

		if newIdx.Name == backends.ReservedMetadataIndexName {
			msg := fmt.Sprintf("The index name %q is reserved by DumboDB", backends.ReservedMetadataIndexName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, msg, command)
		}

		// mongod checks the name before the key's field count.
		if len(newIdx.Key) > backends.MaxIndexKeyFields {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTooManyCompoundKeys, "too many compound keys", command)
		}

		// Compare against earlier entries in the same request.
		for j := i - 1; j >= 0; j-- {
			if identical, err := indexConflict(command, newIdx, toCreate[j], true); err != nil {
				return nil, err
			} else if identical {
				msg := fmt.Sprintf("Identical index already exists: %s", toCreate[j].Name)
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexAlreadyExists, msg, command)
			}
		}

		identical := newKey == "_id: 1" && !timeSeries
		for _, existingIdx := range existing {
			if identical {
				break
			}
			var err error
			if identical, err = indexConflict(command, newIdx, existingIdx, false); err != nil {
				return nil, err
			}
		}
		if !identical {
			filteredToCreate = append(filteredToCreate, newIdx)
		}
	}

	// Check that at most one text index exists (or will exist) per collection.
	existingHasText := false
	for _, idx := range existing {
		if isTextIndex(idx) {
			existingHasText = true
			break
		}
	}

	newTextCount := 0
	for _, idx := range toCreate {
		if isTextIndex(idx) {
			newTextCount++
		}
	}

	if newTextCount > 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"only one text index per collection is allowed",
			command,
		)
	}

	if existingHasText && newTextCount > 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"only one text index per collection is allowed",
			command,
		)
	}

	return filteredToCreate, nil
}

func isTextIndex(idx backends.IndexInfo) bool {
	for _, kp := range idx.Key {
		if kp.Text {
			return true
		}
	}

	return false
}
