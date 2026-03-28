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

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/handler/handlerparams"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// MsgCreateIndexes implements `createIndexes` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgCreateIndexes(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	command := document.Command()

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
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
				handlererrors.ErrIndexesWrongType,
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

	toCreate, err := processIndexesArray(command, idxArr)
	if err != nil {
		return nil, err
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

	numIndexesBefore := len(beforeCreate.Indexes)

	// for compatibility
	if numIndexesBefore == 0 {
		numIndexesBefore = 1
	}

	if toCreate, err = validateIndexesForCreation(command, beforeCreate.Indexes, toCreate); err != nil {
		return nil, err
	}

	_, err = c.CreateIndexes(connCtx, &backends.CreateIndexesParams{Indexes: toCreate})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	resp := new(types.Document)

	resp.Set("numIndexesBefore", int32(numIndexesBefore))
	resp.Set("numIndexesAfter", int32(numIndexesBefore+len(toCreate)))

	if len(toCreate) > 0 {
		resp.Set("createdCollectionAutomatically", createCollection)
	} else {
		resp.Set("note", "all indexes already exist")
	}

	resp.Set("ok", float64(1))

	return documentOpMsg(
		resp,
	)
}

// processIndexesArray processes the given array of indexes and returns a slice of backends.IndexInfo elements.
func processIndexesArray(command string, indexesArray *types.Array) ([]backends.IndexInfo, error) {
	iter := indexesArray.Iterator()
	defer iter.Close()

	res := make([]backends.IndexInfo, indexesArray.Len())

	for {
		var key, val any
		key, val, err := iter.Next()

		switch {
		case err == nil:
			// do nothing
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

		indexInfo, err := processIndex(command, indexDoc)
		if err != nil {
			return nil, err
		}

		res[key.(int)] = *indexInfo
	}
}

// processIndex processes the given index document and returns backends.IndexInfo.
func processIndex(command string, indexDoc *types.Document) (*backends.IndexInfo, error) {
	var index backends.IndexInfo

	iter := indexDoc.Iterator()
	defer iter.Close()

	var hasValue bool

	for {
		opt, _, err := iter.Next()

		switch {
		case err == nil:
			// do nothing
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

			return &index, nil
		default:
			return nil, lazyerrors.Error(err)
		}

		hasValue = true

		// Process required param "key"
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
			// already processed, do nothing

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
			v := must.NotFail(indexDoc.Get("sparse"))
			if sparse, ok := v.(bool); ok && sparse {
				index.Sparse = true
			}

		case "weights", "default_language", "language_override", "textIndexVersion",
			"2dsphereIndexVersion":
			// Index options — accepted but not stored in the backend for now.

		case "partialFilterExpression":
			// Store the partial filter expression so that unique enforcement can
			// exclude documents that don't satisfy the filter.
			if pfe, ok := must.NotFail(indexDoc.Get("partialFilterExpression")).(*types.Document); ok {
				index.PartialFilterExpression = pfe
				// Capture pfe for the closure so it survives loop iteration.
				captured := pfe
				index.MatchesPartialFilter = func(doc *types.Document) (bool, error) {
					return common.FilterDocument(doc, captured)
				}
			}

		case "expireAfterSeconds", "hidden", "storageEngine",
			"bits", "min", "max", "bucketSize", "collation", "wildcardProjection":
			// Accepted but not enforced — stored index behaves as a regular index.
			// TTL expiry, collation enforcement, etc. are not yet implemented.

		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("Index option %q is unknown", opt),
				command,
			)
		}
	}
}

// processIndexKey processes the document containing the index key (set of "field-order" pairs).
func processIndexKey(command string, keyDoc *types.Document) ([]backends.IndexKeyPair, error) {
	res := make([]backends.IndexKeyPair, 0, keyDoc.Len())

	keyIter := keyDoc.Iterator()
	defer keyIter.Close()

	duplicateChecker := make(map[string]struct{}, keyDoc.Len())

	for {
		field, order, err := keyIter.Next()

		switch {
		case err == nil:
			// do nothing
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

		// Check for string-valued index key types: "text", "2dsphere", "2d", "hashed".
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
				fmt.Sprintf("Index key value %q is not implemented yet", orderParam),
				command,
			)
		}

		res = append(res, backends.IndexKeyPair{
			Field:      field,
			Descending: descending,
		})
	}
}

// formatIndexKey formats the given index key to a string.
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

// validateIndexesForCreation validates the given list of indexes to create against the existing ones.
// It filters out duplicate indexes and returns a slice of indexes to create.
// It returns an error if at least one provided index has an invalid specification.
func validateIndexesForCreation(command string, existing, toCreate []backends.IndexInfo) ([]backends.IndexInfo, error) {
	filteredToCreate := make([]backends.IndexInfo, len(toCreate))
	copy(filteredToCreate, toCreate)

	for i, newIdx := range toCreate {
		newKey := formatIndexKey(newIdx.Key)

		if newIdx.Name == "" {
			msg := fmt.Sprintf(
				"Error in specification { key: { %s }, name: %q, v: 2 } :: caused by :: index name cannot be empty",
				newKey, newIdx.Name,
			)

			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCannotCreateIndex, msg, command)
		}

		if newIdx.Name == backends.DefaultIndexName && newKey != "_id: 1" {
			msg := fmt.Sprintf(
				"The index name '_id_' is reserved for the _id index, which must have key pattern {_id: 1},"+
					" found key: { %s }",
				newKey,
			)

			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, msg, command)
		}

		// Iterate backwards to check if the current index is a duplicate of any other index provided in the list earlier.
		for j := i - 1; j >= 0; j-- {
			otherKey := formatIndexKey(toCreate[j].Key)
			otherName := toCreate[j].Name

			if otherName == newIdx.Name && otherKey == newKey {
				msg := fmt.Sprintf("Identical index already exists: %s", otherName)

				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexAlreadyExists, msg, command)
			}

			if newIdx.Name == otherName {
				msg := fmt.Sprintf(
					"An existing index has the same name as the requested index."+
						" When index names are not specified, they are auto generated and can cause conflicts."+
						" Please refer to our documentation. Requested index: { v: 2, key: { %s }, name: %q },"+
						" existing index: { v: 2, key: { %s }, name: %q }",
					newKey, newIdx.Name, otherKey, otherName,
				)

				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexKeySpecsConflict, msg, command)
			}

			if newKey == otherKey {
				msg := fmt.Sprintf(
					"Index already exists with a different name: %s", otherName,
				)

				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOptionsConflict, msg, command)
			}
		}

		// Check for conflicts with existing indexes.
		for _, existingIdx := range existing {
			existingKey := formatIndexKey(existingIdx.Key)

			if (newIdx.Name == existingIdx.Name && newKey == existingKey) || newKey == "_id: 1" {
				// Fully identical indexes are ignored, no need to attempt to create them.
				filteredToCreate = slices.Delete(filteredToCreate, i, i+1)
				break
			}

			if newIdx.Name == existingIdx.Name {
				msg := fmt.Sprintf(
					"An existing index has the same name as the requested index."+
						" When index names are not specified, they are auto generated and can cause conflicts."+
						" Please refer to our documentation. Requested index: { v: 2, key: { %s }, name: %q },"+
						" existing index: { v: 2, key: { %s }, name: %q }",
					newKey, newIdx.Name, existingKey, existingIdx.Name,
				)

				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexKeySpecsConflict, msg, command)
			}

			if newKey == existingKey {
				msg := fmt.Sprintf("Index already exists with a different name: %s", existingIdx.Name)
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOptionsConflict, msg, command)
			}
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

// isTextIndex returns true if the given index has at least one text key field.
func isTextIndex(idx backends.IndexInfo) bool {
	for _, kp := range idx.Key {
		if kp.Text {
			return true
		}
	}

	return false
}
