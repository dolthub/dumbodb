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

package common

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// DuplicateFieldError returns the error for a command repeating the field at
// path, given as its keys: mongod's error where it parses the repeated keys
// as paths or sort keys, and FailedToParse otherwise.
func DuplicateFieldError(path []string) error {
	last := path[len(path)-1]

	if stage := pipelineStage(path); stage >= 0 {
		name, rest := path[stage], path[stage+1:]
		if err := expressionDuplicateError(name, rest); err != nil {
			return err
		}
		switch {
		case (name == "$set" || name == "$addFields") && isFieldPath(rest):
			p := strings.Join(rest, ".")
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrAddFieldsPathCollision,
				fmt.Sprintf("Invalid %s :: caused by :: specification contains two conflicting paths. Cannot specify both '%s' and '%s'",
					name, p, p), name+" (stage)")
		case name == "$project" && isFieldPath(rest):
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrUnsetPathOverwrite,
				"Invalid $project :: caused by :: Path collision at "+last, "$project (stage)")
		case name == "$sort" && len(rest) == 1:
			return sortDuplicateError(last)
		case name == "$listClusterCatalog" && len(rest) == 1:
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIDLDuplicateField,
				fmt.Sprintf("BSON field '$listClusterCatalog.%s' is a duplicate field", last), "$listClusterCatalog (stage)")
		case name == "$setWindowFields" && len(rest) == 2 && rest[0] == "output":
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrWindowOutputPathCollision,
				"$setWindowFields 'output' specification contains two conflicting paths", "$setWindowFields (stage)")
		}
	} else {
		switch {
		case len(path) == 2 && path[0] == "sort":
			return sortDuplicateError(last)
		case len(path) >= 2 && path[0] == "projection" && isFieldPath(path[1:]):
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrUnsetPathOverwrite,
				"Path collision at "+last, "projection")
		}
	}

	return handlererrors.NewCommandErrorMsg(handlererrors.ErrFailedToParse,
		fmt.Sprintf("BSON field '%s' is a duplicate field", strings.Join(path, ".")))
}

// pipelineStage returns the position in path of the innermost pipeline stage
// holding the repeated key, or -1: the first stage of a pipeline, then the
// stages of the $lookup, $unionWith or $facet sub-pipelines below it.
func pipelineStage(path []string) int {
	stage := -1
	for k := 1; k < len(path); k++ {
		if isIndex(path[k-1]) && strings.HasPrefix(path[k], "$") {
			stage = k
			break
		}
	}
	for stage >= 0 {
		rest := path[stage+1:]
		var sub int
		switch {
		case (path[stage] == "$lookup" || path[stage] == "$unionWith") && len(rest) >= 3 && rest[0] == "pipeline":
			sub = stage + 3
		case path[stage] == "$facet" && len(rest) >= 3:
			sub = stage + 3
		default:
			return stage
		}
		if !isIndex(path[sub-1]) || !strings.HasPrefix(path[sub], "$") {
			return stage
		}
		stage = sub
	}
	return stage
}

// namedArgumentOperators lists the expression operators whose object argument
// holds named arguments rather than an expression.
var namedArgumentOperators = map[string]bool{
	"$accumulator": true, "$function": true, "$bottom": true, "$bottomN": true, "$top": true, "$topN": true,
	"$firstN": true, "$lastN": true, "$maxN": true, "$minN": true, "$median": true, "$percentile": true,
	"$cond": true, "$convert": true, "$dateAdd": true, "$dateDiff": true, "$dateFromParts": true,
	"$dateFromString": true, "$dateSubtract": true, "$dateToParts": true, "$dateToString": true, "$dateTrunc": true,
	"$filter": true, "$getField": true, "$let": true, "$ltrim": true, "$map": true, "$reduce": true,
	"$regexFind": true, "$regexFindAll": true, "$regexMatch": true, "$replaceAll": true, "$replaceOne": true,
	"$rtrim": true, "$setField": true, "$sortArray": true, "$switch": true, "$trim": true, "$unsetField": true,
	"$zip": true, "$shift": true, "$derivative": true, "$integral": true, "$expMovingAvg": true,
	"$year": true, "$month": true, "$week": true, "$dayOfMonth": true, "$dayOfWeek": true, "$dayOfYear": true,
	"$hour": true, "$minute": true, "$second": true, "$millisecond": true, "$isoWeek": true,
	"$isoWeekYear": true, "$isoDayOfWeek": true,
}

// expressionDuplicateError returns mongod's error for a key repeated in an
// expression of the stage, at rest below the stage name, or nil where the
// key is not in an expression mongod parses that way.
func expressionDuplicateError(stage string, rest []string) error {
	var prefix string
	var expr []string
	switch stage {
	case "$replaceWith":
		expr = rest
	case "$replaceRoot":
		if len(rest) < 2 || rest[0] != "newRoot" {
			return nil
		}
		expr = rest[1:]
	case "$set", "$addFields", "$project":
		// Nested field names lead to the field's value.
		fields := 0
		for fields < len(rest)-1 && !strings.HasPrefix(rest[fields], "$") && !isIndex(rest[fields]) {
			fields++
		}
		if fields == 0 || fields == len(rest)-1 && !strings.HasPrefix(rest[fields], "$") {
			return nil
		}
		prefix = "Invalid " + stage + " :: caused by :: "
		expr = rest[fields:]
		if len(expr) == 1 {
			if stage == "$project" {
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFieldPathInvalidName,
					prefix+"FieldPath field names may not start with '$'. Consider using $getField or $setField.", stage+" (stage)")
			}
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrAddFieldsExpressionWrongAmountOfArgs,
				prefix+"an expression specification must contain exactly one field, the name of the expression. Found 2 fields",
				stage+" (stage)")
		}
	case "$group":
		if len(rest) < 2 || rest[0] == "_id" || !strings.HasPrefix(rest[1], "$") {
			return nil
		}
		if len(rest) == 2 {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrStageGroupMultipleAccumulator,
				"The field '"+rest[0]+"' must specify one accumulator", "$group (stage)")
		}
		expr = rest[1:]
	default:
		return nil
	}
	if len(expr) == 0 {
		return nil
	}
	for i, key := range expr[:len(expr)-1] {
		if key == "$literal" || namedArgumentOperators[key] && !isIndex(expr[i+1]) && !strings.HasPrefix(expr[i+1], "$") {
			return nil
		}
	}
	if strings.HasPrefix(expr[len(expr)-1], "$") {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrExpressionWrongLenOfFields,
			prefix+"An object representing an expression must have exactly one field", stage+" (stage)")
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrGroupDuplicateFieldName,
		prefix+"duplicate field name specified in object literal", stage+" (stage)")
}

func sortDuplicateError(key string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortDuplicateKey,
		fmt.Sprintf("$sort key must not contain duplicate keys (duplicate: '%s')", key), "$sort")
}

// isFieldPath reports whether keys name a path of fields, entering no
// operator or array.
func isFieldPath(keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "$") || isIndex(k) {
			return false
		}
	}
	return true
}

func isIndex(key string) bool {
	_, err := strconv.Atoi(key)
	return err == nil
}

// findDuplicatePath returns the keys of the path, below prefix, to the first
// field value repeats at any depth, and whether there was one.
func findDuplicatePath(value any, prefix []string) ([]string, bool) {
	switch v := value.(type) {
	case *types.Document:
		if key, ok := v.FindDuplicateKey(); ok {
			return append(prefix[:len(prefix):len(prefix)], key), true
		}
		values := v.Values()
		for i, key := range v.Keys() {
			if path, ok := findDuplicatePath(values[i], append(prefix[:len(prefix):len(prefix)], key)); ok {
				return path, true
			}
		}
	case *types.Array:
		for i := range v.Len() {
			if path, ok := findDuplicatePath(must.NotFail(v.Get(i)), append(prefix[:len(prefix):len(prefix)], strconv.Itoa(i))); ok {
				return path, true
			}
		}
	}
	return nil, false
}
