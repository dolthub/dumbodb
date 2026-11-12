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

// Package stages provides aggregation stages.
package stages

import (
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// newStageFunc is a type for a function that creates a new aggregation stage.
type newStageFunc func(stage *types.Document) (aggregations.Stage, error)

// Stages maps all supported aggregation Stages.
var Stages = map[string]newStageFunc{
	// sorted alphabetically
	"$addFields":         newAddFields,
	"$bucket":            newBucket,
	"$bucketAuto":        newBucketAuto,
	"$changeStream":      newChangeStream,
	"$collStats":         newCollStats,
	"$count":             newCount,
	"$currentOp":         newCurrentOp,
	"$densify":           newDensify,
	"$documents":         newDocumentsOnCollection,
	"$facet":             newFacet,
	"$fill":              newFill,
	"$geoNear":           newGeoNear,
	"$group":             newGroup,
	"$limit":             newLimit,
	"$listLocalSessions": newListLocalSessions,
	"$listSearchIndexes": newListSearchIndexes,
	"$listSessions":      newListSessions,
	"$match":             newMatch,
	"$project":           newProject,
	"$redact":            newRedact,
	"$replaceRoot":       newReplaceRoot,
	"$replaceWith":       newReplaceWith,
	"$sample":            newSample,
	"$search":            newSearch,
	"$searchMeta":        newSearch,
	"$set":               newSet,
	"$setWindowFields":   newSetWindowFields,
	"$skip":              newSkip,
	"$sort":              newSort,
	"$sortByCount":       newSortByCount,
	"$unset":             newUnset,
	"$unwind":            newUnwind,
	"$vectorSearch":      newSearch,
	// please keep sorted alphabetically
}

// IsKnownStage reports whether name is a pipeline stage mongod recognizes.
func IsKnownStage(name string) bool {
	_, ok := Stages[name]
	return ok || specialStages[name]
}

// specialStages are built by msg_aggregate.go, which gives them access to
// the database, rather than by NewStage.
var specialStages = map[string]bool{
	"$graphLookup":    true,
	"$indexStats":     true,
	"$lookup":         true,
	"$merge":          true,
	"$out":            true,
	"$planCacheStats": true,
	"$unionWith":      true,
}

func NewStage(stage *types.Document) (aggregations.Stage, error) {
	if stage.Len() != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageInvalid,
			"A pipeline stage specification object must contain exactly one field.",
			"aggregate",
		)
	}

	name := stage.Command()

	if f, supported := Stages[name]; supported {
		return f(stage)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrStageUnrecognized,
		fmt.Sprintf("Unrecognized pipeline stage name: '%s'", name),
		name+" (stage)", // to differentiate update operator $set from aggregation stage $set, etc
	)
}

// newDocumentsOnCollection is mongod's error for $documents where a
// collection supplies the input; the database-level aggregate handles a
// leading $documents before stages are built.
func newDocumentsOnCollection(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrInvalidNamespace,
		"'$documents' can only be run with {aggregate: 1}",
		"$documents (stage)",
	)
}
