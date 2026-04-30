package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRebaseDebug(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dbg%d", rand.Int64N(1_000_000))

	db := env.client.Database(dbName)
	require.NoError(t, db.Drop(ctx))

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	hashC1 := dumboDBCommit(t, env, dbName, "initial")

	var branchResult bson.M
	err = env.client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchResult)
	require.NoError(t, err)

	_, err = env.client.Database(dbName+"@feature").Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: int32(2)}})
	require.NoError(t, err)
	hashC2 := dumboDBCommit(t, env, dbName+"@feature", "feature-adds-2")

	_, err = db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "v", Value: int32(3)}})
	require.NoError(t, err)
	hashC3 := dumboDBCommit(t, env, dbName, "main-adds-3")

	t.Logf("C1=%s C2=%s C3=%s", hashC1, hashC2, hashC3)

	featureDB := env.client.Database(dbName + "@feature")
	raw := runCommandRaw(t, featureDB, bson.D{
		{Key: "doltRebase", Value: int32(1)},
		{Key: "onto", Value: "main"},
	})

	b, _ := json.Marshal(raw)
	t.Logf("Rebase result: %s", string(b))
}
