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

	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
)

// newStageFunc is a type for a function that creates a new aggregation stage.
type newStageFunc func(stage *types.Document) (aggregations.Stage, error)

// Stages maps all supported aggregation Stages.
var Stages = map[string]newStageFunc{
	// sorted alphabetically
	"$addFields":   newAddFields,
	"$bucket":      newBucket,
	"$bucketAuto":  newBucketAuto,
	"$collStats":   newCollStats,
	"$count":       newCount,
	"$facet":       newFacet,
	"$geoNear":     newGeoNear,
	"$group":       newGroup,
	"$limit":       newLimit,
	"$match":       newMatch,
	"$project":     newProject,
	"$redact":      newRedact,
	"$replaceRoot": newReplaceRoot,
	"$replaceWith": newReplaceWith,
	"$set":              newSet,
	"$sample":           newSample,
	"$setWindowFields":  newSetWindowFields,
	"$skip":             newSkip,
	"$sort":             newSort,
	"$sortByCount":      newSortByCount,
	"$unset":            newUnset,
	"$unwind":           newUnwind,
	// please keep sorted alphabetically
}

// unsupportedStages maps all unsupported yet stages.
var unsupportedStages = map[string]struct{}{
	// sorted alphabetically
	"$changeStream":           {},
	"$currentOp":              {},
	"$densify":                {},
	"$documents":              {},
	"$fill":                   {},
	// $geoNear is now implemented in stages.go
	// $graphLookup is handled specially in msg_aggregate.go via NewGraphLookupStage
	// $indexStats is handled specially in msg_aggregate.go via NewIndexStatsStage
	"$listLocalSessions":      {},
	"$listSessions":           {},
	"$lookup": {}, // handled specially in msg_aggregate.go via NewLookupStage
	// $merge and $out handled specially in msg_aggregate.go via NewMergeStage/NewOutStage
	"$planCacheStats":         {},
	"$search":                 {},
	"$searchMeta":             {},
	// "$setWindowFields" is now implemented above

	"$sharedDataDistribution": {},
	"$unionWith":              {},
	// please keep sorted alphabetically
}

// NewStage creates a new aggregation stage.
func NewStage(stage *types.Document) (aggregations.Stage, error) {
	if stage.Len() != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageInvalid,
			"A pipeline stage specification object must contain exactly one field.",
			"aggregate",
		)
	}

	name := stage.Command()

	f, supported := Stages[name]
	_, unsupported := unsupportedStages[name]

	switch {
	case supported && unsupported:
		panic(fmt.Sprintf("stage %q is in both `stages` and `unsupportedStages`", name))

	case supported && !unsupported:
		return f(stage)

	case !supported && unsupported:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			fmt.Sprintf("`aggregate` stage %q is not implemented yet", name),
			name+" (stage)", // to differentiate update operator $set from aggregation stage $set, etc
		)

	case !supported && !unsupported:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageUnrecognized,
			fmt.Sprintf("Unrecognized pipeline stage name: '%s'", name),
			name+" (stage)", // to differentiate update operator $set from aggregation stage $set, etc
		)
	}

	panic("not reached")
}
