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

package operators

import (
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// metaNames are MongoDB's DocumentMetadataFields names.
var metaNames = map[string]bool{
	"geoNearDistance":         true,
	"geoNearPoint":            true,
	"indexKey":                true,
	"randVal":                 true,
	"recordId":                true,
	"score":                   true,
	"scoreDetails":            true,
	"searchHighlights":        true,
	"searchScore":             true,
	"searchScoreDetails":      true,
	"searchSequenceToken":     true,
	"sortKey":                 true,
	"textScore":               true,
	"timeseriesBucketMaxTime": true,
	"timeseriesBucketMinTime": true,
	"vectorSearchScore":       true,
}

// ParseMetaName follows ExpressionMeta::parse for the operand of $meta.
func ParseMetaName(operand any) (string, error) {
	name, ok := operand.(string)
	if !ok {
		return "", handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMetaNotString,
			"$meta only supports string arguments", "$meta")
	}
	if !metaNames[name] {
		return "", handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMetaUnsupported,
			"Unsupported $meta field: "+name, "$meta")
	}
	if name == "score" || name == "scoreDetails" {
		return "", handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"'featureFlagRankFusionFull' must be enabled to use 'score' or 'scoreDetails' meta field", "$meta")
	}
	return name, nil
}

// metaOp represents { $meta: <name> }.
type metaOp struct {
	name string
}

func newMeta(args ...any) (Operator, error) {
	name, err := ParseMetaName(args[0])
	if err != nil {
		return nil, err
	}
	return &metaOp{name: name}, nil
}

// Process returns the missing value: DumboDB attaches no metadata to
// documents in a pipeline.
func (op *metaOp) Process(*types.Document) (any, error) {
	return nil, aggregations.ErrMissingValue
}

var _ Operator = (*metaOp)(nil)
