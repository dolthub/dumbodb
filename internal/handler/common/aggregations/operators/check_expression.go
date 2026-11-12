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
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// systemVariables are the variables defined in every expression.
var systemVariables = map[string]bool{
	"ROOT": true, "CURRENT": true, "REMOVE": true, "NOW": true, "CLUSTER_TIME": true,
	"USER_ROLES": true, "IS_MR": true, "SEARCH_META": true,
}

// RedactVariables are the variables $redact defines for its expression.
var RedactVariables = []string{"KEEP", "PRUNE", "DESCEND"}

// CheckFieldPaths returns mongod's parse error for the first malformed field
// path or undefined variable in the expression expr, given the variables
// bound around it.
func CheckFieldPaths(expr any, bound ...string) error {
	scope := map[string]bool{}
	for _, name := range bound {
		scope[name] = true
	}
	return checkExpression(expr, scope)
}

func checkExpression(expr any, scope map[string]bool) error {
	switch v := expr.(type) {
	case string:
		if strings.HasPrefix(v, "$") {
			return checkPathString(v, scope)
		}
	case *types.Array:
		for i := range v.Len() {
			if err := checkExpression(must.NotFail(v.Get(i)), scope); err != nil {
				return err
			}
		}
	case *types.Document:
		switch v.Command() {
		case "$literal", "$const":
			return nil
		case "$let":
			return checkLet(v, scope)
		case "$map", "$filter":
			return checkIterating(v, scope)
		case "$reduce":
			return checkReduce(v, scope)
		case "$setField", "$unsetField":
			if spec := operatorSpec(v); spec != nil {
				field, _ := spec.Get("field")
				if name, ok := field.(string); ok && strings.IndexByte(name, 0) >= 0 {
					return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSetFieldNulByte,
						"$setField: 'field' cannot contain an embedded null byte", v.Command())
				}
			}
		}
		for _, k := range v.Keys() {
			if err := checkExpression(must.NotFail(v.Get(k)), scope); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkPathString checks "$path" and "$$variable.path" references.
func checkPathString(v string, scope map[string]bool) error {
	if v == "$" {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrGroupInvalidFieldPath,
			"'$' by itself is not a valid FieldPath", "$")
	}
	path := v[1:]
	if strings.HasPrefix(v, "$$") {
		name, rest, dotted := strings.Cut(v[2:], ".")
		if err := checkVariableReference(name, scope); err != nil {
			return err
		}
		if !dotted {
			return nil
		}
		path = name + "." + rest
	}
	if strings.HasSuffix(path, ".") {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidFieldPath,
			"FieldPath must not end with a '.'.", v)
	}
	const advice = "Consider using $getField or $setField for a field path with '.' or '$'. :: caused by :: "
	for i, part := range strings.Split(path, ".") {
		switch {
		case part == "":
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrPathContainsEmptyElement,
				advice+"FieldPath field names may not be empty strings.", v)
		case strings.HasPrefix(part, "$") && !(i == 0 && strings.HasPrefix(v, "$$")):
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFieldPathInvalidName,
				advice+"FieldPath field names may not start with '$', given '"+part+"'.", v)
		}
	}
	return nil
}

func checkVariableReference(name string, scope map[string]bool) error {
	switch {
	case name == "":
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"empty variable names are not allowed", "$$")
	case scope[name]:
		return nil
	case name == "SEARCH_META":
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSearchMetaWithoutSearch,
			"Can't access $$SEARCH_META without a $search stage earlier in the pipeline", "$$SEARCH_META")
	case !systemVariables[name]:
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrGroupUndefinedVariable,
			"Use of undefined variable: "+name, "$$"+name)
	}
	return nil
}

// CheckUserVariableName is mongod's check of a name an expression defines.
func CheckUserVariableName(name string) error {
	if name == "" {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"empty variable names are not allowed", name)
	}
	if name == "CURRENT" {
		return nil
	}
	if c := name[0]; c < 0x80 && !(c >= 'a' && c <= 'z') {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			fmt.Sprintf("'%s' starts with an invalid character for a user variable name", name), name)
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if c < 0x80 && !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
				fmt.Sprintf("'%s' contains an invalid character for a variable name: '%c'", name, c), name)
		}
	}
	return nil
}

func withVariables(scope map[string]bool, names ...string) map[string]bool {
	inner := make(map[string]bool, len(scope)+len(names))
	for k := range scope {
		inner[k] = true
	}
	for _, n := range names {
		inner[n] = true
	}
	return inner
}

// operatorSpec returns the document argument of a binding operator, or nil
// when it has another shape, which the operator itself reports.
func operatorSpec(v *types.Document) *types.Document {
	spec, _ := must.NotFail(v.Get(v.Command())).(*types.Document)
	return spec
}

func checkLet(v *types.Document, scope map[string]bool) error {
	spec := operatorSpec(v)
	if spec == nil {
		return nil
	}
	var names []string
	varsVal, _ := spec.Get("vars")
	if vars, ok := varsVal.(*types.Document); ok {
		for _, name := range vars.Keys() {
			if err := CheckUserVariableName(name); err != nil {
				return err
			}
			if err := checkExpression(must.NotFail(vars.Get(name)), scope); err != nil {
				return err
			}
			names = append(names, name)
		}
	}
	if in, err := spec.Get("in"); err == nil {
		return checkExpression(in, withVariables(scope, names...))
	}
	return nil
}

func checkIterating(v *types.Document, scope map[string]bool) error {
	spec := operatorSpec(v)
	if spec == nil {
		return nil
	}
	name := "this"
	if as, err := spec.Get("as"); err == nil {
		if s, ok := as.(string); ok {
			if err := CheckUserVariableName(s); err != nil {
				return err
			}
			name = s
		}
	}
	inner := withVariables(scope, name)
	for _, k := range spec.Keys() {
		switch k {
		case "as":
		case "in", "cond":
			if err := checkExpression(must.NotFail(spec.Get(k)), inner); err != nil {
				return err
			}
		default:
			if err := checkExpression(must.NotFail(spec.Get(k)), scope); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkReduce(v *types.Document, scope map[string]bool) error {
	spec := operatorSpec(v)
	if spec == nil {
		return nil
	}
	inner := withVariables(scope, "this", "value")
	for _, k := range spec.Keys() {
		s := scope
		if k == "in" {
			s = inner
		}
		if err := checkExpression(must.NotFail(spec.Get(k)), s); err != nil {
			return err
		}
	}
	return nil
}
