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
	"errors"
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// remotesCollection is the admin-database collection that stores remote
// definitions, one document per remote keyed by "<db>.<remote>".
const remotesCollection = "system.remotes"

// DumboDBRemote adds, lists, or removes a named remote for a database. Remote
// definitions live in admin.system.remotes as documents keyed by "<db>.<name>",
// mirroring the identity model of system.users / system.roles.
func (b *Backend) DumboDBRemote(ctx context.Context, params *backends.RemoteParams) (*backends.RemoteResult, error) {
	adminDB, err := b.Database("admin")
	if err != nil {
		return nil, err
	}

	coll, err := adminDB.Collection(remotesCollection)
	if err != nil {
		return nil, err
	}

	switch params.Action {
	case "add":
		return b.remoteAdd(ctx, coll, params)
	case "list":
		return b.remoteList(ctx, coll, params)
	case "remove":
		return b.remoteRemove(ctx, coll, params)
	default:
		return nil, fmt.Errorf("dumboRemote: unknown action %q (expected add, list, or remove)", params.Action)
	}
}

func (b *Backend) remoteAdd(ctx context.Context, coll backends.Collection, params *backends.RemoteParams) (*backends.RemoteResult, error) {
	if err := validateRemoteName(params.Name); err != nil {
		return nil, err
	}

	ru, err := parseRemoteURL(params.URL)
	if err != nil {
		return nil, fmt.Errorf("dumboRemote add: %w", err)
	}

	id := remoteID(params.DBName, params.Name)

	existing, err := findRemoteDoc(ctx, coll, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("dumboRemote add: remote %q already exists for database %q", params.Name, params.DBName)
	}

	doc := must.NotFail(types.NewDocument(
		"_id", id,
		"name", params.Name,
		"db", params.DBName,
		"url", ru.Raw,
	))

	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		return nil, err
	}

	return &backends.RemoteResult{Remotes: []backends.RemoteInfo{{Name: params.Name, URL: ru.Raw}}}, nil
}

func (b *Backend) remoteList(ctx context.Context, coll backends.Collection, params *backends.RemoteParams) (*backends.RemoteResult, error) {
	qr, err := coll.Query(ctx, &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("db", params.DBName)),
	})
	if err != nil {
		return nil, err
	}
	defer qr.Iter.Close()

	out := []backends.RemoteInfo{}
	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, err
		}

		// The backend filter may be a superset; re-check the db field.
		if dbv, _ := doc.Get("db"); dbv != params.DBName {
			continue
		}

		name, _ := doc.Get("name")
		url, _ := doc.Get("url")
		ns, _ := name.(string)
		us, _ := url.(string)
		out = append(out, backends.RemoteInfo{Name: ns, URL: us})
	}

	return &backends.RemoteResult{Remotes: out}, nil
}

func (b *Backend) remoteRemove(ctx context.Context, coll backends.Collection, params *backends.RemoteParams) (*backends.RemoteResult, error) {
	if params.Name == "" {
		return nil, fmt.Errorf("dumboRemote remove: remote name is required")
	}

	res, err := coll.DeleteAll(ctx, &backends.DeleteAllParams{
		IDs: []any{remoteID(params.DBName, params.Name)},
	})
	if err != nil {
		return nil, err
	}
	if res.Deleted == 0 {
		return nil, fmt.Errorf("dumboRemote remove: remote %q not found for database %q", params.Name, params.DBName)
	}

	return &backends.RemoteResult{}, nil
}

// remoteID builds the db-qualified document key for a remote.
func remoteID(dbName, name string) string {
	return dbName + "." + name
}

// findRemoteDoc returns the remote document with the given _id, or nil if none.
func findRemoteDoc(ctx context.Context, coll backends.Collection, id string) (*types.Document, error) {
	qr, err := coll.Query(ctx, &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("_id", id)),
	})
	if err != nil {
		return nil, err
	}
	defer qr.Iter.Close()

	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		if idv, _ := doc.Get("_id"); idv == id {
			return doc, nil
		}
	}

	return nil, nil
}

// remoteExists reports whether a remote is registered for the database.
func (b *Backend) remoteExists(ctx context.Context, dbName, name string) (bool, error) {
	adminDB, err := b.Database("admin")
	if err != nil {
		return false, err
	}
	coll, err := adminDB.Collection(remotesCollection)
	if err != nil {
		return false, err
	}
	doc, err := findRemoteDoc(ctx, coll, remoteID(dbName, name))
	if err != nil {
		return false, err
	}
	return doc != nil, nil
}

// validateRemoteName rejects empty names and the two characters reserved
// elsewhere in the identity/branch encoding.
func validateRemoteName(name string) error {
	if name == "" {
		return fmt.Errorf("dumboRemote: remote name is required")
	}
	if strings.Contains(name, "@") {
		return fmt.Errorf("dumboRemote: remote name must not contain '@' (reserved as the database/branch delimiter)")
	}
	if strings.ContainsAny(name, " \t\n") {
		return fmt.Errorf("dumboRemote: remote name must not contain whitespace")
	}
	return nil
}
