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

import "context"

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
}

// CommitInfo represents a single commit entry returned by DongoLog.
type CommitInfo struct {
	Hash      string
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
}
