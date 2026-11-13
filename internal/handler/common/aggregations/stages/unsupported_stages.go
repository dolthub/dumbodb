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
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
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
func newCurrentOp(stage *types.Document) (aggregations.Stage, error) {
	if err := CheckCurrentOpSpec(stage); err != nil {
		return nil, err
	}
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

func newListSampledQueries(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIllegalOperation,
		"$listSampledQueries is not supported on a standalone mongod", "$listSampledQueries (stage)")
}

func newSetVariableFromSubPipeline(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrNotAllowedInUserRequests,
		"$setVariableFromSubPipeline' is not allowed in user requests", "$setVariableFromSubPipeline (stage)")
}

func newListCachedAndActiveUsers(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrQueryFeatureNotAllowed,
		"$listCachedAndActiveUsers is not allowed with the current configuration. "+
			"You may need to enable the corresponding feature flag", "$listCachedAndActiveUsers (stage)")
}

// newChangeStreamSplitLargeEvent is mongod's error for the stage outside a
// $changeStream pipeline, which a standalone server never runs.
func newChangeStreamSplitLargeEvent(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIllegalOperation,
		"$changeStreamSplitLargeEvent can only be used in a $changeStream pipeline", "$changeStreamSplitLargeEvent (stage)")
}

// newQuerySettings is mongod's error for $querySettings on a collection; on
// {aggregate: 1} it returns no settings, as DumboDB has none.
func newQuerySettings(stage *types.Document) (aggregations.Stage, error) {
	if err := CheckQuerySettingsSpec(stage); err != nil {
		return nil, err
	}
	return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace,
		"'$querySettings' can only be run with {aggregate: 1}", "$querySettings (stage)")
}

func newListClusterCatalog(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrListClusterCatalogOnCollection,
		"The $listClusterCatalog stage must run on a database, not a collection.", "$listClusterCatalog (stage)")
}

// newInhibitOptimization builds $_internalInhibitOptimization, which only
// stops mongod's optimizer and passes documents through.
func newInhibitOptimization(stage *types.Document) (aggregations.Stage, error) {
	v := must.NotFail(stage.Get("$_internalInhibitOptimization"))
	spec, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
			"$_internalInhibitOptimization must take a nested object but found: $_internalInhibitOptimization: "+
				types.FormatAnyValue(v), "$_internalInhibitOptimization (stage)")
	}
	if spec.Len() > 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"$_internalInhibitOptimization must take an empty object but found: "+types.FormatAnyValue(spec),
			"$_internalInhibitOptimization (stage)")
	}
	return passThroughStage{}, nil
}

func newShardedDataDistribution(*types.Document) (aggregations.Stage, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrShardedDataDistributionMongosOnly,
		"The $shardedDataDistribution stage can only be run on mongoS",
		"$shardedDataDistribution (stage)",
	)
}

// newQueryStats rejects $queryStats outside {aggregate: 1} on admin, which
// msg_aggregate.go handles.
func newQueryStats(stage *types.Document) (aggregations.Stage, error) {
	if err := CheckQueryStatsSpec(stage); err != nil {
		return nil, err
	}
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrInvalidNamespace,
		"$queryStats must be run against the 'admin' database with {aggregate: 1}",
		"$queryStats (stage)",
	)
}

// CheckQueryStatsSpec returns mongod's error for a $queryStats
// specification its IDL parser refuses.
func CheckQueryStatsSpec(stage *types.Document) error {
	v := must.NotFail(stage.Get("$queryStats"))
	spec, ok := v.(*types.Document)
	if !ok {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"$queryStats value must be an object. Found: "+handlerparams.AliasFromType(v), "$queryStats (stage)")
	}
	for _, key := range spec.Keys() {
		val := must.NotFail(spec.Get(key))
		if key != "transformIdentifiers" {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIDLUnknownField,
				fmt.Sprintf("BSON field '$queryStats.%s' is an unknown field.", key), "$queryStats (stage)")
		}
		if _, isNull := val.(types.NullType); isNull {
			continue
		}
		ti, ok := val.(*types.Document)
		if !ok {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
				fmt.Sprintf("BSON field '$queryStats.transformIdentifiers' is the wrong type '%s', expected type 'object'",
					handlerparams.AliasFromType(val)), "$queryStats (stage)")
		}
		algorithm, hasKey, keyLen := "", false, 0
		for _, k := range ti.Keys() {
			tv := must.NotFail(ti.Get(k))
			switch k {
			case "algorithm":
				s, ok := tv.(string)
				if !ok {
					return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
						fmt.Sprintf("BSON field '$queryStats.transformIdentifiers.algorithm' is the wrong type '%s', expected type 'string'",
							handlerparams.AliasFromType(tv)), "$queryStats (stage)")
				}
				if s != "hmac-sha-256" {
					return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
						fmt.Sprintf("Enumeration value '%s' for field '$queryStats.transformIdentifiers.algorithm' is not a valid value.", s),
						"$queryStats (stage)")
				}
				algorithm = s
			case "hmacKey":
				key, ok := tv.(types.Binary)
				if !ok {
					return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
						fmt.Sprintf("BSON field '$queryStats.transformIdentifiers.hmacKey' is the wrong type '%s', expected type 'binData'",
							handlerparams.AliasFromType(tv)), "$queryStats (stage)")
				}
				if key.Subtype != binarySensitive {
					return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
						fmt.Sprintf("BSON field '$queryStats.transformIdentifiers.hmacKey' is the wrong binData type '%s', expected type 'sensitive'",
							binarySubtypeName(key.Subtype)), "$queryStats (stage)")
				}
				hasKey, keyLen = true, len(key.B)
			default:
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIDLUnknownField,
					fmt.Sprintf("BSON field '$queryStats.transformIdentifiers.%s' is an unknown field.", k), "$queryStats (stage)")
			}
		}
		if algorithm == "" {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMissingField,
				"BSON field '$queryStats.transformIdentifiers.algorithm' is missing but a required field", "$queryStats (stage)")
		}
		if !hasKey {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
				"The 'hmacKey' parameter of the $queryStats stage must be specified when applying the hmac-sha-256 algorithm",
				"$queryStats (stage)")
		}
		if keyLen < 32 {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
				fmt.Sprintf("hmacKey must be greater than or equal to 32 bytes, found length: %d", keyLen), "$queryStats (stage)")
		}
	}
	return nil
}

// binarySensitive is the binData subtype mongod requires of an hmacKey.
const binarySensitive = types.BinarySubtype(0x08)

// binarySubtypeName is mongod's name for a binData subtype.
func binarySubtypeName(subtype types.BinarySubtype) string {
	switch subtype {
	case types.BinaryGeneric:
		return "general"
	case types.BinaryFunction:
		return "function"
	case types.BinaryGenericOld:
		return "binary"
	case types.BinaryUUIDOld:
		return "uuid_old"
	case types.BinaryUUID:
		return "uuid"
	case types.BinaryMD5:
		return "md5"
	case types.BinaryEncrypted:
		return "encrypt"
	case types.BinaryUser:
		return "user"
	}
	return fmt.Sprintf("%d", subtype)
}

// CheckQuerySettingsSpec returns mongod's error for a $querySettings
// specification its parser refuses.
func CheckQuerySettingsSpec(stage *types.Document) error {
	spec, ok := must.NotFail(stage.Get("$querySettings")).(*types.Document)
	if !ok {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrQuerySettingsNotObject,
			"$querySettings stage expects a document as argument", "$querySettings (stage)")
	}
	for _, key := range spec.Keys() {
		v := must.NotFail(spec.Get(key))
		if key != "showDebugQueryShape" {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIDLUnknownField,
				fmt.Sprintf("BSON field '$querySettings.%s' is an unknown field.", key), "$querySettings (stage)")
		}
		if _, ok := v.(bool); !ok {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
				"Field 'showDebugQueryShape' should be a boolean value, but found: "+handlerparams.AliasFromType(v),
				"$querySettings (stage)")
		}
	}
	return nil
}

// CheckListCatalogSpec returns mongod's error for a $listCatalog
// specification other than an empty object.
func CheckListCatalogSpec(stage *types.Document) error {
	if spec, ok := must.NotFail(stage.Get("$listCatalog")).(*types.Document); ok && spec.Len() == 0 {
		return nil
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrListCatalogSpec,
		"The $listCatalog stage specification must be an empty object", "$listCatalog (stage)")
}

// newListCatalog rejects $listCatalog, whose catalog entries DumboDB does
// not have.
func newListCatalog(stage *types.Document) (aggregations.Stage, error) {
	if err := CheckListCatalogSpec(stage); err != nil {
		return nil, err
	}
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrNotImplemented,
		"$listCatalog is not implemented",
		"$listCatalog (stage)",
	)
}

// parseFailingStages are the stages that DumboDB, like a standalone mongod,
// refuses in any position while the pipeline is parsed.
var parseFailingStages = map[string]newStageFunc{
	"$changeStream":               newChangeStream,
	"$listSearchIndexes":          newListSearchIndexes,
	"$listSessions":               newListSessions,
	"$listSampledQueries":         newListSampledQueries,
	"$setVariableFromSubPipeline": newSetVariableFromSubPipeline,
	"$listCachedAndActiveUsers":   newListCachedAndActiveUsers,
	"$queryStats":                 newQueryStats,
	"$search":                     newSearch,
	"$searchMeta":                 newSearch,
	"$shardedDataDistribution":    newShardedDataDistribution,
	"$vectorSearch":               newSearch,
}

// CheckViewPipelineStages is CheckParseFailingStages for a view definition,
// which may hold the search stages.
func CheckViewPipelineStages(pipeline *types.Array) error {
	if pipeline == nil {
		return nil
	}
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		name := stage.Command()
		if IsSearchStage(name) {
			continue
		}
		if newStage, fails := parseFailingStages[name]; fails {
			_, err := newStage(stage)
			return err
		}
		if FailsParsing(stage) {
			return nil
		}
	}
	return nil
}

// IsSearchStage reports whether name is an Atlas Search stage.
func IsSearchStage(name string) bool {
	switch name {
	case "$search", "$searchMeta", "$vectorSearch":
		return true
	}
	return false
}

// CheckCurrentOpSpec returns mongod's error for a $currentOp specification
// that is not an object.
func CheckCurrentOpSpec(stage *types.Document) error {
	if _, ok := must.NotFail(stage.Get("$currentOp")).(*types.Document); !ok {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
			"$currentOp options must be specified in an object, but found: "+handlerparams.AliasFromType(must.NotFail(stage.Get("$currentOp"))),
			"$currentOp (stage)")
	}
	return nil
}

// ViewNamespaceError returns mongod's error for a stage at position i of a
// view definition that runs only on {aggregate: 1}, or nil.
func ViewNamespaceError(stage *types.Document, i int) error {
	var err error
	switch stage.Command() {
	case "$documents":
		msg := "'$documents' cannot be used in a view definition"
		if i == 0 {
			msg = "'$documents' can only be run with {aggregate: 1}"
		}
		err = handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "$documents (stage)")
	case "$currentOp":
		_, err = newCurrentOp(stage)
	case "$listLocalSessions":
		_, err = newListLocalSessions(stage)
	case "$listClusterCatalog":
		_, err = newListClusterCatalog(stage)
	case "$querySettings":
		_, err = newQuerySettings(stage)
	}
	return err
}

// CheckParseFailingStages returns the error of the first stage of pipeline
// that fails in any position, as mongod reports it when it parses the
// pipeline, before checks that depend on where the pipeline runs.
func CheckParseFailingStages(pipeline *types.Array) error {
	if pipeline == nil {
		return nil
	}
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		name := stage.Command()
		if newStage, fails := parseFailingStages[name]; fails {
			_, err := newStage(stage)
			return err
		}
		if FailsParsing(stage) {
			// mongod parses stages in order, so this stage's own error,
			// which building the pipeline reports, comes first.
			return nil
		}
	}
	return nil
}

// checkSubPipelineParses returns the error of the first stage of a
// sub-pipeline that fails to parse on its own, nested sub-pipelines
// included when fetcher can build their stages.
func checkSubPipelineParses(pipeline *types.Array, fetcher CollectionFetcher) error {
	if pipeline == nil {
		return nil
	}
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		var err error
		switch name := stage.Command(); {
		case fetcher != nil && (name == "$lookup" || name == "$unionWith" || name == "$facet"):
			_, err = newSubPipelineStage(stage.DeepCopy(), fetcher, false)
		case FailsParsing(stage):
			if newStage, known := stageConstructors[name]; known {
				_, err = newStage(stage.DeepCopy())
			} else {
				err = handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrStageUnrecognized,
					fmt.Sprintf("Unrecognized pipeline stage name: '%s'", name), name+" (stage)")
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// stageConstructors is Stages, set at init to break the cycle through the
// stages that check their sub-pipelines.
var stageConstructors map[string]newStageFunc

func init() {
	stageConstructors = Stages
}

// FailsParsing reports whether a stage fails to parse on its own: an
// unknown stage, or one whose specification its constructor refuses.
// Stages whose checks depend on where they run are not tried.
func FailsParsing(stage *types.Document) bool {
	name := stage.Command()
	if specialStages[name] {
		return false
	}
	newStage, known := stageConstructors[name]
	if !known {
		return true
	}
	switch name {
	case "$documents", "$collStats", "$currentOp", "$listLocalSessions", "$listCatalog", "$facet",
		"$querySettings", "$listClusterCatalog", "$changeStreamSplitLargeEvent":
		return false
	}
	_, err := newStage(stage.DeepCopy())
	return err != nil
}
