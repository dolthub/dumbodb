//go:build ignore

// create_scorecard_user creates the standard FerretDB integration test user
// in a running docudolt server. Used by ferretdb-scorecard.sh to bootstrap auth
// before running the integration test suite.
//
// Usage: go run scripts/create_scorecard_user.go [mongodb-url]
//
// Defaults to mongodb://127.0.0.1:27017/ if no URL is provided.
// On success exits 0. On failure prints the error and exits 1.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	uri := "mongodb://127.0.0.1:27017/"
	if len(os.Args) > 1 {
		uri = os.Args[1]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer client.Disconnect(ctx) //nolint:errcheck

	if err = client.Ping(ctx, nil); err != nil {
		fmt.Fprintf(os.Stderr, "ping: %v\n", err)
		os.Exit(1)
	}

	res := client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "username"},
		{Key: "pwd", Value: "password"},
		{Key: "roles", Value: bson.A{
			bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}},
		}},
	})
	if res.Err() != nil {
		fmt.Fprintf(os.Stderr, "createUser: %v\n", res.Err())
		os.Exit(1)
	}

	fmt.Println("Scorecard user 'username' created.")
}
