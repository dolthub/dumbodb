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

package users

import (
	"context"
	"errors"

	"github.com/FerretDB/wire/wirebson"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
	"github.com/dolthub/dumbodb/internal/util/password"
)

// CreateUserParams represents the parameters of CreateUser function.
//
//nolint:vet // for readability
type CreateUserParams struct {
	Database   string
	Username   string
	Password   password.Password
	Mechanisms *types.Array
	Roles      *types.Array
}

// CreateUser stores a new user in the given backend.
func CreateUser(ctx context.Context, b backends.Backend, params *CreateUserParams) error {
	must.NotBeZero(params)

	credentials, err := MakeCredentials(params.Username, params.Password, params.Mechanisms)
	if err != nil {
		return err
	}

	roles, err := NormalizeRoles(params.Roles, params.Database)
	if err != nil {
		return err
	}

	id := uuid.New()
	saved := must.NotFail(types.NewDocument(
		"_id", params.Database+"."+params.Username,
		"credentials", credentials,
		"user", params.Username,
		"db", params.Database,
		"roles", roles,
		"userId", types.Binary{Subtype: types.BinaryUUID, B: must.NotFail(id.MarshalBinary())},
	))

	db := must.NotFail(b.Database("admin"))
	coll := must.NotFail(db.Collection("system.users"))

	_, err = coll.InsertAll(ctx, &backends.InsertAllParams{
		Docs: []*types.Document{saved},
	})

	return err
}

// NormalizeRoles converts a createUser/updateUser roles array into the canonical
// stored form: an array of {role, db} documents. A shorthand string entry resolves
// its db to defaultDB (the database the command runs against). Roles are stored for
// MongoDB compatibility but not enforced. A nil input yields an empty array.
func NormalizeRoles(roles *types.Array, defaultDB string) (*types.Array, error) {
	if roles == nil {
		return types.MakeArray(0), nil
	}

	out := types.MakeArray(roles.Len())

	iter := roles.Iterator()
	defer iter.Close()

	for {
		_, v, err := iter.Next()

		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		switch r := v.(type) {
		case string:
			out.Append(must.NotFail(types.NewDocument("role", r, "db", defaultDB)))
		case *types.Document:
			role, _ := r.Get("role")
			db, _ := r.Get("db")

			roleStr, ok := role.(string)
			if !ok {
				return nil, lazyerrors.Errorf("role name must be a string, got %T", role)
			}

			dbStr, ok := db.(string)
			if !ok {
				return nil, lazyerrors.Errorf("role db must be a string, got %T", db)
			}

			out.Append(must.NotFail(types.NewDocument("role", roleStr, "db", dbStr)))
		default:
			return nil, lazyerrors.Errorf("unsupported role entry type %T", v)
		}
	}

	return out, nil
}

// MakeCredentials creates a document with credentials for the chosen mechanisms.
// The mechanisms array must be validated by the caller.
func MakeCredentials(username string, userPassword password.Password, mechanisms *types.Array) (*types.Document, error) {
	credentials := types.MakeDocument(0)

	if mechanisms == nil {
		mechanisms = must.NotFail(types.NewArray("SCRAM-SHA-1", "SCRAM-SHA-256"))
	}

	iter := mechanisms.Iterator()
	defer iter.Close()

	for {
		var v any
		_, v, err := iter.Next()

		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		var hash *wirebson.Document
		var hashDoc *types.Document

		switch v {
		case "SCRAM-SHA-1":
			if hash, err = password.SCRAMSHA1VariationHash(username, userPassword); err != nil {
				return nil, err
			}

			if hashDoc, err = bson.ToDocument(hash); err != nil {
				return nil, lazyerrors.Error(err)
			}

			credentials.Set("SCRAM-SHA-1", hashDoc)
		case "SCRAM-SHA-256":
			if hash, err = password.SCRAMSHA256Hash(userPassword); err != nil {
				return nil, err
			}

			if hashDoc, err = bson.ToDocument(hash); err != nil {
				return nil, lazyerrors.Error(err)
			}

			credentials.Set("SCRAM-SHA-256", hashDoc)
		default:
			panic("unknown mechanism")
		}
	}

	return credentials, nil
}
