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
	"errors"
	"fmt"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
)

// ErrEmptyCommit is returned by VersioningBackend.DumboDBCommit when the working set
// has no changes versus HEAD and CommitParams.AllowEmpty is false.
var ErrEmptyCommit = errors.New("nothing to commit, working tree clean")

type CommitParams struct {
	DBName     string
	Branch     string
	Message    string
	Author     string    // required: name of the commit author
	Timestamp  time.Time // optional: commit timestamp; zero value means use current time
	AllowEmpty bool      // if true, create a commit even when the working set has no changes vs HEAD
}

type CommitResult struct {
	CommitID           string
	Branch             string
	Message            string
	Author             string // echoes CommitParams.Author
	Timestamp          int64  // Unix milliseconds of the commit timestamp (author date)
	Committer          string // "Name <email>" of the committer; equals Author for regular commits
	CommitterTimestamp int64  // Unix milliseconds of the committer date
}

type BranchParams struct {
	DBName string
	From   string // source branch to branch from (current connection branch); also used to detect current-branch delete
	Name   string // name of the new branch (or branch to delete when Delete is true)
	Delete bool   // if true, delete the named branch instead of creating it
	Force  bool   // if true together with Delete, skip the unmerged-commits safety check (forceDelete semantics)
}

type BranchResult struct {
	Branch string
}

type MergeParams struct {
	DBName   string
	Into     string // target branch (the current branch)
	From     string // source branch to merge from
	Abort    bool   // if true, abort the in-progress merge and restore the pre-merge state
	Continue bool   // if true, resume after conflict resolution and create the merge commit
	Message  string // optional: custom merge commit message (ignored on fast-forward and already-up-to-date)
	Author   string // optional: 'Name <email>' for the merge commit author
	NoFF     bool   // if true, force a merge commit even when fast-forward is possible
	FFOnly   bool   // if true, fail with ErrOperationFailed if a fast-forward is not possible
}

type MergeResult struct {
	CommitID           string
	Message            string
	Author             string
	Timestamp          int64 // Unix milliseconds (author date)
	Committer          string
	CommitterTimestamp int64 // Unix milliseconds (committer date)
}

// ConflictSummary summarizes the number of document conflicts in one collection.
type ConflictSummary struct {
	Collection string
	Count      int
}

// MergeConflictError is returned by DumboDBMerge when the merge cannot be completed
// automatically due to conflicting document changes on both branches. The merge is staged
// but not committed; conflicts must be resolved via DumboDBResolveConflict before
// DumboDBCommit will succeed.
type MergeConflictError struct {
	Conflicts []ConflictSummary
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("dumboMerge: unresolved conflicts in %d collection(s)", len(e.Conflicts))
}

type CherryPickParams struct {
	DBName    string
	Branch    string // current branch (the target branch to apply the cherry-pick onto)
	Commit    string // rootish of the commit to cherry-pick (required unless Abort/Continue)
	Abort     bool   // if true, abandon the in-progress cherry-pick and restore working set
	Continue  bool   // if true, after conflict resolution, complete the cherry-pick and create the commit
	Message   string // optional: custom commit message override
	Committer string // optional: 'Name <email>' explicit committer identity; when empty, committer equals the original author
}

type CherryPickResult struct {
	CommitID           string
	Message            string
	Author             string
	Timestamp          int64 // Unix milliseconds (author date)
	Committer          string
	CommitterTimestamp int64 // Unix milliseconds (committer date)
}

// DumboDBCherryPickConflictError is returned by DumboDBCherryPick when the cherry-pick
// cannot be completed automatically due to conflicting document changes. The cherry-pick
// is staged but not committed; conflicts must be resolved via DumboDBResolveConflict
// before DumboDBCherryPick continue will succeed.
type DumboDBCherryPickConflictError struct {
	Conflicts []ConflictSummary
}

func (e *DumboDBCherryPickConflictError) Error() string {
	return fmt.Sprintf("dumboCherryPick: unresolved conflicts in %d collection(s)", len(e.Conflicts))
}

// ConflictInfo describes a single document-level conflict in an in-progress merge.
type ConflictInfo struct {
	ConflictID    string
	Base          *types.Document // nil when the document was absent in the common ancestor (added by one or both branches)
	Ours          *types.Document // nil when our branch deleted the document
	Theirs        *types.Document // nil when their branch deleted the document
	OurDiffType   string          // "added", "modified", "deleted"
	TheirDiffType string          // "added", "modified", "deleted"
}

type ConflictsParams struct {
	DBName string
	Branch string
}

// CollectionConflicts groups conflicts for a single collection.
type CollectionConflicts struct {
	Collection string
	Conflicts  []ConflictInfo
}

// ConflictsResult represents the result of VersioningBackend.DumboDBConflicts method.
// Returns all conflict details for all collections.
type ConflictsResult struct {
	Collections []CollectionConflicts
}

type ResolveConflictParams struct {
	DBName     string
	Branch     string
	Collection string
	ConflictID string
	Resolution string          // "ours", "theirs", or "custom"
	Value      *types.Document // only used when Resolution == "custom"
}

type ResolveConflictResult struct{}

type LogParams struct {
	DBName     string
	Branch     string
	ConnBranch string   // branch name from the connection's encoded db name (used for HEAD -> decoration)
	Limit      int32
	From       []string // optional: seed commit hashes for the traversal frontier; empty means start at HEAD
	Stat       bool     // when true, include per-collection change counts for each commit
	Patch      bool     // when true, include full document-level diffs for each commit
	All        bool     // when true, seed the walk with every branch HEAD (mutually exclusive with From)
}

// CommitInfo represents a single commit entry returned by DumboDBLog.
type CommitInfo struct {
	CommitID           string
	Parent1            string   // empty for root commit (no parent)
	Parent2            string   // non-empty only for merge commits
	Author             string
	Email              string
	Message            string
	Timestamp          int64    // Unix milliseconds (author date)
	Committer          string   // "Name <email>" of the committer; equals Author when not explicitly set
	CommitterTimestamp int64    // Unix milliseconds (committer date)
	Refs               []string // branch/tag decorations; empty when commit is not a branch head
	Stat               []TableStatus    // per-collection change summary (only when LogParams.Stat is true)
	Diff               []CollectionDiff // full document diffs (only when LogParams.Patch is true)
}

type LogResult struct {
	Commits []CommitInfo
	// Next is the frontier seed set for the following page (the commits discovered
	// but not yet examined). Empty when the traversal is exhausted.
	Next []string
}

type VersioningStatusParams struct {
	DBName string
	Branch string
}

// TableStatus represents the change status of a single table in a versioning status result.
//
// Added, Modified, and Deleted are document-level counts of uncommitted changes in the
// collection, computed by diffing the working set against HEAD:
//   - Added: documents present in the working set but not at HEAD
//   - Modified: documents present in both but with differing content
//   - Deleted: documents present at HEAD but not in the working set
//
// For a collection with Status == "added", all working-set documents count toward Added
// (HEAD has no copy). For Status == "deleted", all HEAD documents count toward Deleted.
type TableStatus struct {
	Name     string
	Status   string // "added", "modified", or "deleted"
	Added    int
	Modified int
	Deleted  int

	// Index lifecycle, name-only for the brief view. An index appears in
	// ModifiedIndexes when the same name is present on both sides with
	// different definitions -- i.e. drop+recreate within the uncommitted
	// working set. Full per-index detail surfaces through DumboDBDiff.
	//
	// Callers must always emit all three on the wire as empty arrays
	// when there are no changes of that kind, so consumers do not have
	// to branch on field presence.
	AddedIndexes    []string
	ModifiedIndexes []string
	RemovedIndexes  []string
}

type VersioningStatusResult struct {
	Branch    string
	CommitID  string // HEAD commit hash; populated only when the workspace is clean (no changes)
	Tables    []TableStatus
	MergeOp   string            // "merge", "cherry-pick", "rebase", or "revert"; empty when no operation in progress
	Conflicts []ConflictSummary // per-collection conflict counts; empty when no conflicts
}

// DiffParams represents the parameters of VersioningBackend.DumboDBDiff method.
//
// From and To accept rootish expressions (commit hashes, branch names, ancestor
// expressions like "main~2", or "HEAD"/"HEAD~N"). Empty string means the default:
//   - From="": use HEAD of the connection's branch as the "a" side
//   - To="": use the working set (uncommitted state) as the "b" side
//
// ConnRootish is the rootish from the connection's encoded database name (e.g.
// "feature" from "mydb@feature"). It is used to resolve "HEAD" and "HEAD~N" in
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

// IndexChange represents a secondary index that has the same name on
// both sides with a different definition. From and To both carry the
// full IndexInfo. The Name field is implicit (From.Name == To.Name).
type IndexChange struct {
	From IndexInfo
	To   IndexInfo
}

// CollectionDiff represents the changes to a single collection.
//
// The three index slices are symmetric with the three document slices:
// each lists entries of one kind, with full detail per entry. Callers
// must always emit all six on the wire as empty arrays when empty, so
// consumers do not have to branch on field presence.
type CollectionDiff struct {
	Name            string
	Status          string            // "added" (only in b), "deleted" (only in a), or "modified" (in both with doc or index changes)
	Added           []*types.Document // full documents added in "b"
	Removed         []*types.Document // full documents removed from "a"
	Modified        []ModifiedDoc     // documents changed between "a" and "b"
	AddedIndexes    []IndexInfo       // full definitions of indexes added in "b"
	ModifiedIndexes []IndexChange     // pre/post definitions for indexes whose spec changed
	RemovedIndexes  []IndexInfo       // full definitions of indexes removed from "a"
}

// DiffResult represents the result of VersioningBackend.DumboDBDiff method.
// Only collections with at least one change appear.
type DiffResult struct {
	Collections []CollectionDiff
}

type ResetParams struct {
	DBName   string
	Branch   string
	CommitID string
	Hard     bool
}

type ResetResult struct {
	CommitID string
}

type RebaseParams struct {
	DBName    string
	Branch    string // current branch (the branch to rebase)
	Onto      string // branch name or rootish to rebase onto (required unless Abort/Continue)
	Committer string // optional: explicit committer identity for replayed commits ("Name <email>"); when empty, committer equals original author
	Abort     bool   // if true, abandon the in-progress rebase and restore the pre-rebase state
	Continue  bool   // if true, after conflict resolution, complete the current commit and proceed
}

type RebaseResult struct {
	CommitsReplayed int
	NewTip          string // hash of the new branch tip after rebase
}

// DumboDBRebaseConflictError is returned by DumboDBRebase when a commit replay
// cannot be completed automatically due to conflicting document changes. The rebase
// is paused; conflicts must be resolved via DumboDBResolveConflict before
// DumboDBRebase continue will succeed.
type DumboDBRebaseConflictError struct {
	Conflicts      []ConflictSummary
	ConflictCommit string // hash of the commit being replayed when the conflict occurred
}

func (e *DumboDBRebaseConflictError) Error() string {
	return fmt.Sprintf("dumboRebase: unresolved conflicts in %d collection(s) replaying commit %s", len(e.Conflicts), e.ConflictCommit)
}

type RevertParams struct {
	DBName   string
	Branch   string // current branch (the branch to apply the revert onto)
	Commit   string // rootish of the commit to revert (required unless Abort/Continue)
	Abort    bool   // if true, abandon the in-progress revert and restore working set
	Continue bool   // if true, after conflict resolution, complete the revert and create the commit
	Message  string // optional: custom commit message override
	Author   string // optional: 'Name <email>'
}

type RevertResult struct {
	CommitID           string
	Message            string
	Author             string
	Timestamp          int64 // Unix milliseconds (author date)
	Committer          string
	CommitterTimestamp int64 // Unix milliseconds (committer date)
}

// DumboDBRevertConflictError is returned by DumboDBRevert when the revert
// cannot be completed automatically due to conflicting document changes. The revert
// is staged but not committed; conflicts must be resolved via DumboDBResolveConflict
// before DumboDBRevert continue will succeed.
type DumboDBRevertConflictError struct {
	Conflicts []ConflictSummary
}

func (e *DumboDBRevertConflictError) Error() string {
	return fmt.Sprintf("dumboRevert: unresolved conflicts in %d collection(s)", len(e.Conflicts))
}

// TagInfo describes a single tag entry returned by VersioningBackend.DumboDBTag.
type TagInfo struct {
	Name      string
	CommitID  string
	Author    string // tagger identity "Name <email>"; empty when tag has no metadata
	Message   string // tag description/message; empty when tag has no metadata
	Timestamp int64  // Unix milliseconds; zero when tag has no metadata
}

// TagParams represents the parameters of VersioningBackend.DumboDBTag method.
//
// Operation is selected by the combination of fields:
//   - Name == "" and Delete == false   -> list all tags (other fields ignored).
//   - Name != "" and Delete == false   -> create a tag named Name pointing at the commit
//     that Hash resolves to (Hash is a rootish: commit hash, branch name, ancestor expr,
//     or another tag name). If Hash == "", the connection's branch HEAD is used.
//   - Name != "" and Delete == true    -> delete the tag named Name.
type TagParams struct {
	DBName  string
	Branch  string // connection's branch (used to resolve a default Hash on create)
	Name    string // tag name; empty means list-all
	Hash    string // rootish to tag (create only); empty means connection branch HEAD
	Delete  bool   // if true, delete the named tag instead of creating one
	Message string // optional: tag description (create only)
	Author  string // optional: tagger identity "Name <email>" (create only)
}

// TagResult represents the result of VersioningBackend.DumboDBTag method.
//
// For list operations, Tags contains every tag in the database.
// For create/delete, Tags contains a single entry describing the affected tag
// (delete entries have Author/Message empty since the tag is gone).
type TagResult struct {
	Tags []TagInfo
}

// GCMode names the GC strategy: "default" sweeps new-gen and
// unreferenced old-gen chunks; "full" rewrites every chunk (compacts
// old-gen even when nothing is unreferenced).
type GCParams struct {
	DBName string
	Mode   string // "default" (zero value) or "full"
}

type GCResult struct {
	DB           string // resolved base database name (branch selector stripped)
	Mode         string // "default" or "full" -- echoes the effective mode
	DurationMs   int64
	SizeBefore   uint64
	SizeAfter    uint64
	ChunksBefore uint32
	ChunksAfter  uint32
}

// VersioningBackend is an optional interface for backends that support Dolt versioning operations.
// The handler checks for this interface via type assertion; backends that don't implement it
// will cause the dumbodb versioning commands to return an unsupported error.
type VersioningBackend interface {
	// DumboDBCommit commits the current working set on the given branch with the provided message.
	DumboDBCommit(context.Context, *CommitParams) (*CommitResult, error)

	// DumboDBBranch creates a new branch starting from the given source branch.
	DumboDBBranch(context.Context, *BranchParams) (*BranchResult, error)

	// DumboDBMerge merges the source branch (From) into the target branch (Into).
	DumboDBMerge(context.Context, *MergeParams) (*MergeResult, error)

	// DumboDBLog returns the commit history for the given branch.
	DumboDBLog(context.Context, *LogParams) (*LogResult, error)

	// DumboDBStatus returns the uncommitted changes on the given branch.
	DumboDBStatus(context.Context, *VersioningStatusParams) (*VersioningStatusResult, error)

	// DumboDBDiff returns the document-level diff between two states.
	// If From is empty, the "a" side is HEAD. If To is empty, the "b" side is the working set.
	DumboDBDiff(context.Context, *DiffParams) (*DiffResult, error)

	// DumboDBReset moves the branch HEAD to the given commit hash.
	// Soft reset (Hard=false): leaves the working tree unchanged; staged root is updated to the target commit.
	// Hard reset (Hard=true): resets both the working tree and staged root to the target commit,
	// discarding all uncommitted changes.
	DumboDBReset(context.Context, *ResetParams) (*ResetResult, error)

	// DumboDBConflicts returns conflict information for the current in-progress merge on the given branch.
	// If ConflictsParams.Collection is empty, returns a per-collection summary (Collections field).
	// If ConflictsParams.Collection is set, returns per-conflict details for that collection (Conflicts field).
	// Returns ErrOperationFailed if no merge is in progress on the branch.
	DumboDBConflicts(context.Context, *ConflictsParams) (*ConflictsResult, error)

	// DumboDBResolveConflict resolves a single document conflict in the current in-progress merge.
	// Resolution must be "ours", "theirs", or "custom". For "custom", Value provides the document to use.
	// Returns ErrOperationFailed if no merge is in progress, if the collection or conflict ID is not found,
	// or if the conflict is already resolved.
	DumboDBResolveConflict(context.Context, *ResolveConflictParams) (*ResolveConflictResult, error)

	// DumboDBCherryPick applies the diff introduced by the named commit onto the current branch's
	// working set and creates a new commit. The commit parameter is a rootish (commit hash or
	// ancestor expression). On conflict, the cherry-pick is staged but not committed and a
	// *DumboDBCherryPickConflictError is returned. Conflicts are resolved via
	// DumboDBResolveConflict/DumboDBConflicts. After resolution, use Continue=true to complete
	// the cherry-pick. Use Abort=true to abandon an in-progress cherry-pick.
	DumboDBCherryPick(context.Context, *CherryPickParams) (*CherryPickResult, error)

	// DumboDBRebase reapplies all commits on the current branch not reachable from Onto onto the
	// tip of Onto, rewriting branch history. On conflict during a commit replay, the rebase is paused
	// and a *DumboDBRebaseConflictError is returned. Conflicts are resolved via
	// DumboDBResolveConflict/DumboDBConflicts. After resolution, use Continue=true to complete
	// the current commit and proceed. Use Abort=true to restore the pre-rebase state.
	DumboDBRebase(context.Context, *RebaseParams) (*RebaseResult, error)

	// DumboDBRevert applies the inverse diff introduced by the named commit onto the current
	// branch's working set and creates a new commit that undoes those changes. The commit
	// parameter is a rootish (commit hash or ancestor expression). On conflict, the revert is
	// staged but not committed and a *DumboDBRevertConflictError is returned. Conflicts are
	// resolved via DumboDBResolveConflict/DumboDBConflicts. After resolution, use Continue=true
	// to complete the revert. Use Abort=true to abandon an in-progress revert.
	DumboDBRevert(context.Context, *RevertParams) (*RevertResult, error)

	// DumboDBTag creates, lists, or deletes tags. Tags share the same ref namespace as
	// dolt tags (refs/tags/<name>) and use the Dolt tag flatbuffer (TagValue), so tags
	// created here are visible to the dolt tag CLI and vice versa. Operation is selected
	// by the combination of TagParams fields; see TagParams documentation.
	DumboDBTag(context.Context, *TagParams) (*TagResult, error)

	// DumboDBGC runs garbage collection on the database's chunk store.
	// Every branch in the database is in scope (one chunk store per
	// logical database). Mode is "default" (sweep new-gen / unreferenced
	// old-gen) or "full" (rewrite every chunk). Requires a session in
	// ctx; the calling session participates in the GC safepoint as the
	// callSession (excluded from the waited set).
	DumboDBGC(context.Context, *GCParams) (*GCResult, error)
}
