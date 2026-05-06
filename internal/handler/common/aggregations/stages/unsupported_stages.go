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

// newDensify validates the $densify stage spec and returns an appropriate error.
// $densify is not implemented; this handler ensures the correct error code and message
// are returned when the required 'field' field is absent, matching MongoDB's behavior.
func newDensify(stage *types.Document) (aggregations.Stage, error) {
	v, err := stage.Get("$densify")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrIDLFailedToParse,
			"BSON field '$densify.field' is missing but a required field",
			"$densify (stage)",
		)
	}

	spec, ok := v.(*types.Document)
	if !ok || spec == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrIDLFailedToParse,
			"BSON field '$densify.field' is missing but a required field",
			"$densify (stage)",
		)
	}

	if _, err := spec.Get("field"); err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrIDLFailedToParse,
			"BSON field '$densify.field' is missing but a required field",
			"$densify (stage)",
		)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrNotImplemented,
		"`aggregate` stage \"$densify\" is not implemented yet",
		"$densify (stage)",
	)
}

// newFill validates the $fill stage spec and returns an appropriate error.
// $fill is not implemented; this handler ensures the correct error code and message
// are returned when the required 'output' field is absent, matching MongoDB's behavior.
func newFill(stage *types.Document) (aggregations.Stage, error) {
	v, err := stage.Get("$fill")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrIDLFailedToParse,
			"BSON field '$fill.output' is missing but a required field",
			"$fill (stage)",
		)
	}

	spec, ok := v.(*types.Document)
	if !ok || spec == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrIDLFailedToParse,
			"BSON field '$fill.output' is missing but a required field",
			"$fill (stage)",
		)
	}

	if _, err := spec.Get("output"); err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrIDLFailedToParse,
			"BSON field '$fill.output' is missing but a required field",
			"$fill (stage)",
		)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrNotImplemented,
		"`aggregate` stage \"$fill\" is not implemented yet",
		"$fill (stage)",
	)
}

// newSearch returns a SearchNotEnabled error for the $search aggregation stage.
// $search requires Atlas Search which is not available in this deployment.
func newSearch(stage *types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrSearchNotEnabled,
		"Using $search and $vectorSearch aggregation stages requires additional configuration. "+
			"Please connect to Atlas or an AtlasCLI local deployment to enable."+
			"For more information on how to connect, see https://dochub.mongodb.org/core/atlas-cli-deploy-local-reqs.",
		"$search (stage)",
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
