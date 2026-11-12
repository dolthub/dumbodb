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
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// searchNotEnabledMessage is mongod's error for an Atlas Search stage on a
// server without mongot.
const searchNotEnabledMessage = "Using $search and $vectorSearch aggregation stages requires additional configuration. " +
	"Please connect to Atlas or an AtlasCLI local deployment to enable.For more information on how to connect, " +
	"see https://dochub.mongodb.org/core/atlas-cli-deploy-local-reqs."

// newSearch rejects $search, $searchMeta and $vectorSearch as a mongod
// without Atlas Search does.
func newSearch(stage *types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrSearchNotEnabled,
		searchNotEnabledMessage,
		stage.Command()+" (stage)",
	)
}

// newCurrentOp rejects $currentOp outside {aggregate: 1} on admin, which
// msg_aggregate.go handles.
func newCurrentOp(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrInvalidNamespace,
		"$currentOp must be run against the 'admin' database with {aggregate: 1}",
		"$currentOp (stage)",
	)
}

// newListLocalSessions rejects $listLocalSessions on a collection;
// msg_aggregate.go handles {aggregate: 1}.
func newListLocalSessions(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrInvalidNamespace,
		"$listLocalSessions must be run against the database with {aggregate: 1}, not a collection",
		"$listLocalSessions (stage)",
	)
}

// newListSessions rejects $listSessions, which runs only against
// config.system.sessions, a namespace DumboDB does not have.
func newListSessions(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrInvalidNamespace,
		"$listSessions may only be run against config.system.sessions",
		"$listSessions (stage)",
	)
}

func newListSearchIndexes(stage *types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrSearchNotEnabled,
		"Using Atlas Search Database Commands and the $listSearchIndexes aggregation stage requires additional configuration. "+
			"Please connect to Atlas or an AtlasCLI local deployment to enable. For more information on how to connect, "+
			"see https://dochub.mongodb.org/core/atlas-cli-deploy-local-reqs.",
		"$listSearchIndexes (stage)",
	)
}

// newChangeStream returns a ChangeStreamNotSupported error for the $changeStream aggregation stage.
// $changeStream is not supported on standalone servers; MongoDB requires a replica set or sharded cluster.
func newChangeStream(stage *types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrChangeStreamNotSupported,
		"The $changeStream stage is only supported on replica sets",
		"$changeStream (stage)",
	)
}
