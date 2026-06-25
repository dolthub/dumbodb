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

package verify

// These verification tests share the dumbodb test harness and response decoders
// with package tests via the importable package support. The aliases let the
// scenario tests use the same bare names as the manual-verification docs.

import "github.com/dolthub/dumbodb/tests/support"

type (
	dumboDBTestEnv   = support.Env
	statusResult     = support.StatusResult
	tableStatusEntry = support.TableStatusEntry
	logResult        = support.LogResult
	commitEntry      = support.CommitEntry
	bsEntry          = support.BsEntry
)

var (
	repoRoot                = support.RepoRoot
	bsToInt64               = support.BsToInt64
	bsBranchCreate          = support.BsBranchCreate
	bsMerge                 = support.BsMerge
	bsTag                   = support.BsTag
	bsStatus                = support.BsStatus
	bsAssert                = support.BsAssert
	bsNewDB                 = support.BsNewDB
	runLog                  = support.RunLog
	logCommitIDs            = support.LogCommitIDs
	logNext                 = support.LogNext
	assertRootishRejected   = support.AssertRootishRejected
	assertWriteBlockedOperationFailed = support.AssertWriteBlockedOperationFailed
	startDumboDB            = support.StartDumboDB
	dumboDBCommit           = support.Commit
	dumboDBCommitAllowEmpty = support.CommitAllowEmpty
	runCommandRaw           = support.RunCommandRaw
	decodeStatusResult      = support.DecodeStatusResult
	toInt                   = support.ToInt
	findTableStatus         = support.FindTableStatus
	runStatus               = support.RunStatus
	decodeLogResult         = support.DecodeLogResult
)
