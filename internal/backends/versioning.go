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

package backends

import (
	"context"
	"time"

	"github.com/dolthub/dongo/internal/types"
)

// CommitParams represents the parameters of VersioningBackend.DongoCommit method.
type CommitParams struct {
	DBName    string
	Branch    string
	Message   string
	Author    string    // required: name of the commit author
	Timestamp time.Time // optional: commit timestamp; zero value means use current time
}

// CommitResult represents the result of VersioningBackend.DongoCommit method.
type CommitResult struct {
	CommitID  string
	Branch    string
	Message   string
	Author    string // echoes CommitParams.Author
	Timestamp int64  // Unix milliseconds of the commit timestamp
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
	CommitID string
	Message  string
}

// LogParams represents the parameters of VersioningBackend.DongoLog method.
type LogParams struct {
	DBName     string
	Branch     string
	ConnBranch string // branch name from the connection's encoded db name (used for HEAD -> decoration)
	Limit      int32
	From       string // optional: start traversal from this commit hash instead of HEAD
}

// CommitInfo represents a single commit entry returned by DongoLog.
type CommitInfo struct {
	CommitID  string
	Parent1   string   // empty for root commit (no parent)
	Parent2   string   // non-empty only for merge commits
	Author    string
	Email     string
	Message   string
	Timestamp int64    // Unix milliseconds
	Refs      []string // branch/tag decorations; empty when commit is not a branch head
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
//
// From and To accept rootish expressions (commit hashes, branch names, ancestor
// expressions like "main~2", or "HEAD"/"HEAD~N"). Empty string means the default:
//   - From="": use HEAD of the connection's branch as the "a" side
//   - To="": use the working set (uncommitted state) as the "b" side
//
// ConnRootish is the rootish from the connection's encoded database name (e.g.
// "feature" from "mydb__feature"). It is used to resolve "HEAD" and "HEAD~N" in
// From/To: HEAD means the committed tip of ConnRootish, not necessarily main.
type DiffParams struct {
	DBName      string
	ConnRootish string // rootish from the connection's encoded db name (e.g. "main", "feature", "main~2")
	From        string // rootish; empty means HEAD of the connection's branch
	To          string // rootish; empty means working set
}

// FieldDiff represents a single field-level change within a modified document.
type FieldDiff struct {
	Type string // "added", "modified", or "removed"
	Path string // JSON Path (e.g. "$.field", "$.nested.field", "$.array[0]")
	From any    // old value; nil for Type=="added"
	To   any    // new value; nil for Type=="removed"
}

// ModifiedDoc represents a document that was changed between two commits.
// Diff contains the path-based field diffs for all changed fields.
type ModifiedDoc struct {
	ID   any
	Diff []FieldDiff
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
	DBName   string
	Branch   string
	CommitID string
	Hard     bool
}

// ResetResult represents the result of VersioningBackend.DongoReset method.
type ResetResult struct {
	CommitID string
}

// CurrentBranchParams represents the parameters of VersioningBackend.DongoCurrentBranch method.
type CurrentBranchParams struct {
	DBName string
	Branch string
}

// CurrentBranchResult represents the result of VersioningBackend.DongoCurrentBranch method.
type CurrentBranchResult struct {
	Branch string
}

// VersioningBackend is an optional interface for backends that support Dolt versioning operations.
// The handler checks for this interface via type assertion; backends that don't implement it
// will cause the dongo versioning commands to return an unsupported error.
type VersioningBackend interface {
	// DongoCommit commits the current working set on the given branch with the provided message.
	DongoCommit(context.Context, *CommitParams) (*CommitResult, error)

	// DongoBranch creates a new branch starting from the given source branch.
	DongoBranch(context.Context, *BranchParams) (*BranchResult, error)

	// DongoCurrentBranch returns the current branch name for the connection.
	DongoCurrentBranch(context.Context, *CurrentBranchParams) (*CurrentBranchResult, error)

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
