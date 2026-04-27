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

// Package version provides DumboDB build and MongoDB compatibility version info.
package version

import (
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/debugbuild"
	"github.com/dolthub/dumbodb/internal/util/must"
)

const (
	// Version is the DumboDB build version.
	Version = "v0.1.0"

	// MongoDBVersion is the MongoDB version DumboDB advertises to clients
	// that gate behavior on major.minor.
	MongoDBVersion = "8.0.0"

	// Commit is the source git commit. Populated as "unknown" until a real
	// build pipeline injects it.
	Commit = "unknown"
)

// Info provides details about the current build.
//
//nolint:vet // for readability
type Info struct {
	Version          string
	Commit           string
	Branch           string
	Dirty            bool
	Package          string
	DebugBuild       bool
	BuildEnvironment *types.Document

	// MongoDBVersion is fake MongoDB version for clients that check major.minor to adjust their behavior.
	MongoDBVersion string

	// MongoDBVersionArray is MongoDBVersion, but as an array.
	MongoDBVersionArray *types.Array
}

var info = &Info{
	Version:             Version,
	Commit:              Commit,
	Branch:              "unknown",
	Dirty:               false,
	Package:             "unknown",
	DebugBuild:          debugbuild.Enabled,
	BuildEnvironment:    must.NotFail(types.NewDocument()),
	MongoDBVersion:      MongoDBVersion,
	MongoDBVersionArray: must.NotFail(types.NewArray(int32(8), int32(0), int32(0), int32(0))),
}

// Get returns current build's info.
func Get() *Info {
	return info
}
