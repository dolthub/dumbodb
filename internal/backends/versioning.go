// Copyright 2024 Dolt Inc.
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

package backends

import (
	"context"

	"github.com/dolthub/dongo/internal/types"
)

// CommitParams represents the parameters of VersioningBackend.DongoCommit method.
type CommitParams struct {
	DBName  string
	Branch  string
	Message string
}

// CommitResult represents the result of VersioningBackend.DongoCommit method.
type CommitResult struct {
	Hash    string
	Branch  string
	Message string
}

// BranchParams represents the parameters of VersioningBackend.DongoBranch method.
type BranchParams struct {
	DBName string
	From   string // source branch to branch from
	Name   string // name of the new branch
}

// BranchResult represents the result of VersioningBackend.DongoBranch method.
type BranchResult struct {
	Branch string
}

// MergeParams represents the parameters of VersioningBackend.DongoMerge method.
type MergeParams struct {
	DBName string
	Into   string // target branch (the current branch)
	From   string // source branch to merge from
}

// MergeResult represents the result of VersioningBackend.DongoMerge method.
type MergeResult struct {
	Hash    string
	Message string
}

// LogParams represents the parameters of VersioningBackend.DongoLog method.
type LogParams struct {
	DBName string
	Branch string
	Limit  int32
	From   string // optional: start traversal from this commit hash instead of HEAD
}

// CommitInfo represents a single commit entry returned by DongoLog.
type CommitInfo struct {
	Hash      string
	Parent1   string // empty for root commit (no parent)
	Parent2   string // non-empty only for merge commits
	Author    string
	Email     string
	Message   string
	Timestamp int64 // Unix milliseconds
}

// LogResult represents the result of VersioningBackend.DongoLog method.
type LogResult struct {
	Commits []CommitInfo
}

// VersioningStatusParams represents the parameters of VersioningBackend.DongoStatus method.
type VersioningStatusParams struct {
	DBName string
	Branch string
}

// TableStatus represents the change status of a single table in a versioning status result.
type TableStatus struct {
	Name   string
	Status string // "added", "modified", or "deleted"
}

// VersioningStatusResult represents the result of VersioningBackend.DongoStatus method.
type VersioningStatusResult struct {
	Branch string
	Tables []TableStatus
}

// DiffParams represents the parameters of VersioningBackend.DongoDiff method.
// From and To are commit hashes (empty string means default):
//   - From="": use HEAD (committed state) as the "a" side
//   - To="": use the working set (uncommitted state) as the "b" side
type DiffParams struct {
	DBName string
	From   string // commit hash; empty means HEAD
	To     string // commit hash; empty means working set
}

// ModifiedDoc represents a document that was changed between two commits.
// Only fields that differ between the two versions appear in A and B.
type ModifiedDoc struct {
	ID any             // the _id value
	A  *types.Document // old values of changed fields only
	B  *types.Document // new values of changed fields only
}

// CollectionDiff represents the changes to a single collection.
type CollectionDiff struct {
	Name     string
	Added    []*types.Document // full documents added in "b"
	Removed  []*types.Document // full documents removed from "a"
	Modified []ModifiedDoc     // documents changed between "a" and "b"
}

// DiffResult represents the result of VersioningBackend.DongoDiff method.
// Only collections with at least one change appear.
type DiffResult struct {
	Collections []CollectionDiff
}

// ResetParams represents the parameters of VersioningBackend.DongoReset method.
type ResetParams struct {
	DBName string
	Branch string
	Hash   string
	Hard   bool
}

// ResetResult represents the result of VersioningBackend.DongoReset method.
type ResetResult struct {
	Hash string
}

// VersioningBackend is an optional interface for backends that support Dolt versioning operations.
// The handler checks for this interface via type assertion; backends that don't implement it
// will cause the dongo versioning commands to return an unsupported error.
type VersioningBackend interface {
	// DongoCommit commits the current working set on the given branch with the provided message.
	DongoCommit(context.Context, *CommitParams) (*CommitResult, error)

	// DongoBranch creates a new branch starting from the given source branch.
	DongoBranch(context.Context, *BranchParams) (*BranchResult, error)

	// DongoMerge merges the source branch (From) into the target branch (Into).
	DongoMerge(context.Context, *MergeParams) (*MergeResult, error)

	// DongoLog returns the commit history for the given branch.
	DongoLog(context.Context, *LogParams) (*LogResult, error)

	// DongoStatus returns the uncommitted changes on the given branch.
	DongoStatus(context.Context, *VersioningStatusParams) (*VersioningStatusResult, error)

	// DongoDiff returns the document-level diff between two states.
	// If From is empty, the "a" side is HEAD. If To is empty, the "b" side is the working set.
	DongoDiff(context.Context, *DiffParams) (*DiffResult, error)

	// DongoReset moves the branch HEAD to the given commit hash.
	// Soft reset (Hard=false): leaves the working tree unchanged; staged root is updated to the target commit.
	// Hard reset (Hard=true): resets both the working tree and staged root to the target commit,
	// discarding all uncommitted changes.
	DongoReset(context.Context, *ResetParams) (*ResetResult, error)
}
