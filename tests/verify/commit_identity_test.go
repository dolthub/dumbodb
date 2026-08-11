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

package verify

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// identityResult captures the author/committer echoed by VC command responses.
type identityResult struct {
	CommitID  string `bson:"commitId"`
	Author    string `bson:"author"`
	Committer string `bson:"committer"`
}

// codeUnknownField is MongoDB's IDLUnknownField (RejectUnknownFields outcome).
const codeUnknownField = 40415

func TestCommitIdentityRejectsClientIdentity(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")
	adminRun(t, admin, "admin", bson.D{
		{Key: "createUser", Value: "dev"}, {Key: "pwd", Value: "pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
		{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Dev"}, {Key: "email", Value: "dev@corp.io"}}},
	})
	dev := authClient(t, port, "dev", "pw", "admin")

	id := "x <x@y.z>"
	cases := []struct {
		name  string
		cmd   bson.D
		field string
	}{
		{"commit author", bson.D{{Key: "dumboCommit", Value: 1}, {Key: "author", Value: id}}, "author"},
		{"commit committer", bson.D{{Key: "dumboCommit", Value: 1}, {Key: "committer", Value: id}}, "committer"},
		{"merge author", bson.D{{Key: "dumboMerge", Value: 1}, {Key: "mergeIn", Value: "x"}, {Key: "author", Value: id}}, "author"},
		{"merge committer", bson.D{{Key: "dumboMerge", Value: 1}, {Key: "mergeIn", Value: "x"}, {Key: "committer", Value: id}}, "committer"},
		{"revert author", bson.D{{Key: "dumboRevert", Value: 1}, {Key: "commit", Value: "abc"}, {Key: "author", Value: id}}, "author"},
		{"rebase author", bson.D{{Key: "dumboRebase", Value: 1}, {Key: "onto", Value: "main"}, {Key: "author", Value: id}}, "author"},
		{"rebase committer", bson.D{{Key: "dumboRebase", Value: 1}, {Key: "onto", Value: "main"}, {Key: "committer", Value: id}}, "committer"},
		{"cherryPick author", bson.D{{Key: "dumboCherryPick", Value: 1}, {Key: "commit", Value: "abc"}, {Key: "author", Value: id}}, "author"},
		{"cherryPick committer", bson.D{{Key: "dumboCherryPick", Value: 1}, {Key: "commit", Value: "abc"}, {Key: "committer", Value: id}}, "committer"},
		{"tag author", bson.D{{Key: "dumboTag", Value: 1}, {Key: "name", Value: "v1"}, {Key: "author", Value: id}}, "author"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := dev.Database("repo@main").RunCommand(ctx, tc.cmd).Err()
			requireCode(t, err, codeUnknownField)
		})
	}
}

// TestCommitIdentityAuthOffHonorsAuthor confirms the author param still works with --auth off.
func TestCommitIdentityAuthOffHonorsAuthor(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	require.NoError(t, env.Client.Database("repo").RunCommand(ctx, bson.D{
		{Key: "insert", Value: "items"}, {Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: 1}}}},
	}).Err())
	var res identityResult
	require.NoError(t, env.Client.Database("repo").RunCommand(ctx, bson.D{
		{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "m"}, {Key: "author", Value: "Ext Author <ext@x.io>"},
	}).Decode(&res))
	require.Equal(t, "Ext Author <ext@x.io>", res.Author)
	require.Equal(t, "Ext Author <ext@x.io>", res.Committer)
}

// commitIdentityDoc is the {name,email} shape stored per user and echoed by usersInfo.
type commitIdentityDoc struct {
	Name  string `bson:"name"`
	Email string `bson:"email"`
}

func TestCommitIdentityStamping(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")

	rw := bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: "shop"}}}
	adminRun(t, admin, "shop", bson.D{
		{Key: "createUser", Value: "alice"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rw},
		{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Alice Dev"}, {Key: "email", Value: "alice@corp.io"}}},
	})
	adminRun(t, admin, "shop", bson.D{
		{Key: "createUser", Value: "bob"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rw},
	})

	alice := authClient(t, port, "alice", "pw", "shop")
	bob := authClient(t, port, "bob", "pw", "shop")

	insertAndCommit := func(t *testing.T, c *mongo.Client, id int) identityResult {
		t.Helper()
		require.NoError(t, c.Database("shop").RunCommand(ctx, bson.D{
			{Key: "insert", Value: "items"}, {Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: id}}}},
		}).Err())
		var res identityResult
		require.NoError(t, c.Database("shop").RunCommand(ctx, bson.D{
			{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "change"},
		}).Decode(&res))
		return res
	}

	t.Run("commit stamps the stored identity", func(t *testing.T) {
		res := insertAndCommit(t, alice, 1)
		require.Equal(t, "Alice Dev <alice@corp.io>", res.Author)
		require.Equal(t, "Alice Dev <alice@corp.io>", res.Committer)
	})

	t.Run("commit falls back to username@authDb", func(t *testing.T) {
		res := insertAndCommit(t, bob, 2)
		require.Equal(t, "bob <bob@shop>", res.Author)
		require.Equal(t, "bob <bob@shop>", res.Committer)
	})

	t.Run("revert stamps the acting identity, not the reverted commit's", func(t *testing.T) {
		// alice commits, bob reverts it: committer and author are bob (a new commit).
		target := insertAndCommit(t, alice, 3)
		var res identityResult
		require.NoError(t, bob.Database("shop").RunCommand(ctx, bson.D{
			{Key: "dumboRevert", Value: 1}, {Key: "commit", Value: target.CommitID},
		}).Decode(&res))
		require.Equal(t, "bob <bob@shop>", res.Author)
		require.Equal(t, "bob <bob@shop>", res.Committer)
	})

	t.Run("tag stamps the acting identity as tagger", func(t *testing.T) {
		var res identityResult
		require.NoError(t, alice.Database("shop@main").RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: 1}, {Key: "name", Value: "v1"}, {Key: "message", Value: "release"},
		}).Decode(&res))
		require.Equal(t, "Alice Dev <alice@corp.io>", res.Author)
	})
}

func TestCommitIdentityReplayStamping(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")

	// Two root users (root covers branch-qualified DBs) with distinct identities.
	rootRole := bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}
	adminRun(t, admin, "admin", bson.D{
		{Key: "createUser", Value: "aa"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rootRole},
		{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Alice Dev"}, {Key: "email", Value: "alice@corp.io"}}},
	})
	adminRun(t, admin, "admin", bson.D{
		{Key: "createUser", Value: "bb"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rootRole},
		{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Bob Ops"}, {Key: "email", Value: "bob@corp.io"}}},
	})
	aa := authClient(t, port, "aa", "pw", "admin")
	bb := authClient(t, port, "bb", "pw", "admin")

	commit := func(t *testing.T, c *mongo.Client, db, coll string, id int32, msg string) identityResult {
		t.Helper()
		require.NoError(t, c.Database(db).RunCommand(ctx, bson.D{
			{Key: "insert", Value: coll}, {Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: id}}}},
		}).Err())
		var res identityResult
		require.NoError(t, c.Database(db).RunCommand(ctx, bson.D{
			{Key: "dumboCommit", Value: 1}, {Key: "message", Value: msg},
		}).Decode(&res))
		return res
	}

	// aa authors a base commit on main, branches "feature", and authors C2 on feature.
	commit(t, aa, "repo", "items", 1, "base")
	require.NoError(t, aa.Database("repo@main").RunCommand(ctx, bson.D{
		{Key: "dumboBranch", Value: 1}, {Key: "branch", Value: "feature"},
	}).Err())
	c2 := commit(t, aa, "repo@feature", "items", 2, "add-two")
	require.Equal(t, "Alice Dev <alice@corp.io>", c2.Author)

	t.Run("cherry-pick preserves author, stamps actor as committer", func(t *testing.T) {
		var pick identityResult
		require.NoError(t, bb.Database("repo@main").RunCommand(ctx, bson.D{
			{Key: "dumboCherryPick", Value: 1}, {Key: "commit", Value: c2.CommitID},
		}).Decode(&pick))
		require.Equal(t, "Alice Dev <alice@corp.io>", pick.Author, "author preserved from the picked commit")
		require.Equal(t, "Bob Ops <bob@corp.io>", pick.Committer, "committer is the acting user")
	})
}

func TestCommitIdentityAutoCommit(t *testing.T) {
	env := startDumboDB(t, "--auth", "--auto-commit")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")

	rw := bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: "auto"}}}
	adminRun(t, admin, "auto", bson.D{
		{Key: "createUser", Value: "alice"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rw},
		{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Alice Dev"}, {Key: "email", Value: "alice@corp.io"}}},
	})
	adminRun(t, admin, "auto", bson.D{
		{Key: "createUser", Value: "bob"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rw},
	})
	alice := authClient(t, port, "alice", "pw", "auto")
	bob := authClient(t, port, "bob", "pw", "auto")

	headIdentity := func(t *testing.T, c *mongo.Client) (author, committer string) {
		t.Helper()
		var res struct {
			Commits []struct {
				Author    string `bson:"author"`
				Committer string `bson:"committer"`
			} `bson:"commits"`
		}
		require.NoError(t, c.Database("auto").RunCommand(ctx, bson.D{
			{Key: "dumboLog", Value: 1}, {Key: "limit", Value: 1},
		}).Decode(&res))
		require.NotEmpty(t, res.Commits)
		return res.Commits[0].Author, res.Commits[0].Committer
	}

	t.Run("auto-commit stamps the acting identity", func(t *testing.T) {
		require.NoError(t, alice.Database("auto").RunCommand(ctx, bson.D{
			{Key: "insert", Value: "items"}, {Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: 1}}}},
		}).Err())
		author, committer := headIdentity(t, alice)
		require.Equal(t, "Alice Dev <alice@corp.io>", author)
		require.Equal(t, "Alice Dev <alice@corp.io>", committer)
	})

	t.Run("auto-commit falls back to username@authDb", func(t *testing.T) {
		require.NoError(t, bob.Database("auto").RunCommand(ctx, bson.D{
			{Key: "insert", Value: "items"}, {Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: 2}}}},
		}).Err())
		author, committer := headIdentity(t, bob)
		require.Equal(t, "bob <bob@auto>", author)
		require.Equal(t, "bob <bob@auto>", committer)
	})
}

func TestCommitIdentityMergeAndRebase(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")
	rootRole := bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}
	adminRun(t, admin, "admin", bson.D{
		{Key: "createUser", Value: "aa"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rootRole},
		{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Alice Dev"}, {Key: "email", Value: "alice@corp.io"}}},
	})
	adminRun(t, admin, "admin", bson.D{
		{Key: "createUser", Value: "bb"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rootRole},
		{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Bob Ops"}, {Key: "email", Value: "bob@corp.io"}}},
	})
	aa := authClient(t, port, "aa", "pw", "admin")
	bb := authClient(t, port, "bb", "pw", "admin")

	commit := func(t *testing.T, c *mongo.Client, db, coll string, id int32, msg string) identityResult {
		t.Helper()
		require.NoError(t, c.Database(db).RunCommand(ctx, bson.D{
			{Key: "insert", Value: coll}, {Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: id}}}},
		}).Err())
		var res identityResult
		require.NoError(t, c.Database(db).RunCommand(ctx, bson.D{
			{Key: "dumboCommit", Value: 1}, {Key: "message", Value: msg},
		}).Decode(&res))
		return res
	}
	branch := func(t *testing.T, c *mongo.Client, db, name string) {
		t.Helper()
		require.NoError(t, c.Database(db+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: 1}, {Key: "branch", Value: name},
		}).Err())
	}

	t.Run("merge commit is authored and committed by the actor", func(t *testing.T) {
		commit(t, aa, "mrg", "items", 1, "base")
		branch(t, aa, "mrg", "feature")
		commit(t, aa, "mrg", "items", 2, "main-2")         // diverge main
		commit(t, aa, "mrg@feature", "items", 3, "feat-3") // diverge feature

		var res identityResult
		require.NoError(t, bb.Database("mrg@main").RunCommand(ctx, bson.D{
			{Key: "dumboMerge", Value: 1}, {Key: "mergeIn", Value: "feature"}, {Key: "noFF", Value: true},
		}).Decode(&res))
		require.Equal(t, "Bob Ops <bob@corp.io>", res.Author)
		require.Equal(t, "Bob Ops <bob@corp.io>", res.Committer)
	})

	t.Run("rebase preserves replayed author, actor is committer", func(t *testing.T) {
		commit(t, aa, "rbs", "items", 1, "base")
		branch(t, aa, "rbs", "feature")
		commit(t, aa, "rbs", "items", 2, "main-2")         // diverge main
		commit(t, aa, "rbs@feature", "items", 3, "feat-3") // aa authors on feature

		require.NoError(t, bb.Database("rbs@feature").RunCommand(ctx, bson.D{
			{Key: "dumboRebase", Value: 1}, {Key: "onto", Value: "main"},
		}).Err())

		// The rebased HEAD keeps aa's authorship but records bb as committer.
		var logRes struct {
			Commits []struct {
				Author    string `bson:"author"`
				Committer string `bson:"committer"`
			} `bson:"commits"`
		}
		require.NoError(t, bb.Database("rbs@feature").RunCommand(ctx, bson.D{
			{Key: "dumboLog", Value: 1}, {Key: "limit", Value: 1},
		}).Decode(&logRes))
		require.NotEmpty(t, logRes.Commits)
		require.Equal(t, "Alice Dev <alice@corp.io>", logRes.Commits[0].Author, "author preserved")
		require.Equal(t, "Bob Ops <bob@corp.io>", logRes.Commits[0].Committer, "committer is the actor")
	})
}

func TestCommitIdentityUsersInfo(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	// Bootstrap the first admin via the localhost exception.
	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"},
		{Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")

	readIdentity := func(db, user string) *commitIdentityDoc {
		var res struct {
			Users []struct {
				User           string             `bson:"user"`
				CommitIdentity *commitIdentityDoc `bson:"commitIdentity"`
			} `bson:"users"`
		}
		require.NoError(t, admin.Database(db).RunCommand(ctx, bson.D{{Key: "usersInfo", Value: user}}).Decode(&res))
		require.Len(t, res.Users, 1)
		return res.Users[0].CommitIdentity
	}

	rwRole := bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: "appid"}}}

	t.Run("full identity round-trips", func(t *testing.T) {
		adminRun(t, admin, "appid", bson.D{
			{Key: "createUser", Value: "full"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rwRole},
			{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Alice Example"}, {Key: "email", Value: "alice@acme.com"}}},
		})
		id := readIdentity("appid", "full")
		require.NotNil(t, id)
		require.Equal(t, "Alice Example", id.Name)
		require.Equal(t, "alice@acme.com", id.Email)
	})

	t.Run("name-only identity", func(t *testing.T) {
		adminRun(t, admin, "appid", bson.D{
			{Key: "createUser", Value: "nameonly"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rwRole},
			{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Bob"}}},
		})
		id := readIdentity("appid", "nameonly")
		require.NotNil(t, id)
		require.Equal(t, "Bob", id.Name)
		require.Empty(t, id.Email)
	})

	t.Run("no identity", func(t *testing.T) {
		adminRun(t, admin, "appid", bson.D{
			{Key: "createUser", Value: "plain"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rwRole},
		})
		require.Nil(t, readIdentity("appid", "plain"))
	})

	t.Run("invalid email rejected", func(t *testing.T) {
		err := admin.Database("appid").RunCommand(ctx, bson.D{
			{Key: "createUser", Value: "bad"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rwRole},
			{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Bad"}, {Key: "email", Value: "not-an-email"}}},
		}).Err()
		requireCode(t, err, codeBadValue)
	})

	t.Run("updateUser sets, replaces, and clears identity", func(t *testing.T) {
		adminRun(t, admin, "appid", bson.D{
			{Key: "createUser", Value: "mut"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rwRole},
		})
		require.Nil(t, readIdentity("appid", "mut"))

		// set
		adminRun(t, admin, "appid", bson.D{
			{Key: "updateUser", Value: "mut"},
			{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Carol"}, {Key: "email", Value: "carol@acme.com"}}},
		})
		id := readIdentity("appid", "mut")
		require.NotNil(t, id)
		require.Equal(t, "carol@acme.com", id.Email)

		// replace (wholesale)
		adminRun(t, admin, "appid", bson.D{
			{Key: "updateUser", Value: "mut"},
			{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Dave"}, {Key: "email", Value: "dave@acme.com"}}},
		})
		id = readIdentity("appid", "mut")
		require.NotNil(t, id)
		require.Equal(t, "Dave", id.Name)
		require.Equal(t, "dave@acme.com", id.Email)

		// clear (explicit null)
		adminRun(t, admin, "appid", bson.D{
			{Key: "updateUser", Value: "mut"},
			{Key: "commitIdentity", Value: nil},
		})
		require.Nil(t, readIdentity("appid", "mut"))
	})

	t.Run("updateUser rejects malformed identity", func(t *testing.T) {
		err := admin.Database("appid").RunCommand(ctx, bson.D{
			{Key: "updateUser", Value: "full"},
			{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "X<y"}, {Key: "email", Value: "x@y.z"}}},
		}).Err()
		requireCode(t, err, codeBadValue)
	})

	t.Run("commitIdentity survives showCustomData:false", func(t *testing.T) {
		var res struct {
			Users []struct {
				CommitIdentity *commitIdentityDoc `bson:"commitIdentity"`
			} `bson:"users"`
		}
		require.NoError(t, admin.Database("appid").RunCommand(ctx, bson.D{
			{Key: "usersInfo", Value: "full"}, {Key: "showCustomData", Value: false},
		}).Decode(&res))
		require.Len(t, res.Users, 1)
		require.NotNil(t, res.Users[0].CommitIdentity)
		require.Equal(t, "alice@acme.com", res.Users[0].CommitIdentity.Email)
	})
}
