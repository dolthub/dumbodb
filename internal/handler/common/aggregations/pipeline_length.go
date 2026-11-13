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

package aggregations

import (
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// MaxPipelineLength is mongod's internalPipelineLengthLimit: no pipeline,
// sub-pipeline or view may hold more stages. It also bounds every loop over a
// pipeline's stages.
const MaxPipelineLength = 1000

// CheckPipelineLength returns mongod's error for a pipeline of n stages when n
// exceeds MaxPipelineLength.
func CheckPipelineLength(n int) error {
	if n <= MaxPipelineLength {
		return nil
	}
	return handlererrors.NewCommandErrorMsg(handlererrors.ErrPipelineTooLong, "Pipeline length must be no longer than 1000 stages.")
}

// expandingStages are the stages mongod runs as two: $count as a $group and a
// $project, $sortByCount and $bucket as a $group and a $sort.
var expandingStages = map[string]bool{"$count": true, "$sortByCount": true, "$bucket": true}

// ExpandedLength is how many stages mongod runs stages as, once those that
// expand into two are expanded.
func ExpandedLength(stages []any) int {
	n := len(stages)
	for _, s := range stages {
		if d, ok := s.(*types.Document); ok && d.Len() == 1 && expandingStages[d.Command()] {
			n++
		}
	}
	return n
}

// CheckExpandedPipelineLength returns mongod's error for a pipeline whose
// stages, once expanded, are over the limit.
func CheckExpandedPipelineLength(n int) error {
	if n <= MaxPipelineLength {
		return nil
	}
	return handlererrors.NewCommandErrorMsg(handlererrors.ErrViewPipelineTooLong, "Pipeline length must be no longer than 1000 stages")
}
