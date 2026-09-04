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
	Committer  string    // optional: 'Name <email>' committer identity; empty means the committer equals Author
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
	Action string // "add", "update", "remove", or "list"
	From   string // connection rootish: the source for add, and the current-branch guard for remove
	Name   string // branch to add, update, or remove; empty for list
	Force  bool   // remove: skip the unmerged-commits safety check (force delete)

	// ConfigUpdate is applied on add (optional, atomic) and update.
	ConfigUpdate *BranchConfigUpdate
}

// BranchConfigUpdate is a partial change to a branch's config.{pull,push}.
type BranchConfigUpdate struct {
	PullRemote *string
	PullBranch *string
	PullRebase *string
	PullFF     *string
	PushRemote *string
	PushBranch *string
	UnsetPull  bool
	UnsetPush  bool
}

// BranchInfo describes a single branch returned when BranchParams.List is set.
type BranchInfo struct {
	Name     string
	CommitID string // branch HEAD commit hash

	Pull *BranchPullInfo
	Push *BranchPushInfo

	RemoteTracking bool
	Remote         string
	Ref            string
}

// BranchPullInfo is a branch's fetch/merge config.
type BranchPullInfo struct {
	Remote string
	Branch string
	Rebase string
	FF     string
}

// BranchPushInfo is a branch's persistent push target.
type BranchPushInfo struct {
	Remote string
	Branch string
}

type BranchResult struct {
	Branch   string       // name of the created or deleted branch; empty when listing
	Branches []BranchInfo // populated only when BranchParams.List is set, sorted by Name

	Configured bool
	Pull       *BranchPullInfo
	Push       *BranchPushInfo
}

type MergeParams struct {
	DBName    string
	Into      string // target branch (the current branch)
	From      string // source commit-ish to merge from: branch, tag, commit hash, or traversal expression
	Abort     bool   // if true, abort the in-progress merge and restore the pre-merge state
	Continue  bool   // if true, resume after conflict resolution and create the merge commit
	Message   string // optional: custom merge commit message (ignored on fast-forward and already-up-to-date)
	Author    string // optional: 'Name <email>' for the merge commit author
	Committer string // optional: 'Name <email>' committer identity; empty means the committer equals Author
	NoFF      bool   // if true, force a merge commit even when fast-forward is possible
	FFOnly    bool   // if true, fail with ErrOperationFailed if a fast-forward is not possible
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

// ConflictInfo describes a single conflict in an in-progress merge. A
// documentEdit conflict shares one _id across Base/Ours/Theirs; a
// uniqueKeyCollision conflict has Ours and Theirs as distinct identities
// (each document carries its own _id) contending for one indexed key.
type ConflictInfo struct {
	ConflictID    string
	Type          string          // "documentEdit" or "uniqueKeyCollision"
	Base          *types.Document // nil when the document was absent in the common ancestor (added by one or both branches)
	Ours          *types.Document // nil when our branch deleted the document
	Theirs        *types.Document // nil when their branch deleted the document
	OurDiffType   string          // "added", "modified", "deleted"
	TheirDiffType string          // "added", "modified", "deleted"
	Reason        ConflictReason
}

// ConflictReason explains why two states cannot both stand.
type ConflictReason struct {
	Code    string          // e.g. "bothModified", "modifyDelete", "deleteModify", "uniqueKeyCollision"
	Message string          // human-readable explanation
	Index   string          // unique index name, set only for uniqueKeyCollision
	Key     *types.Document // colliding key value, set only for uniqueKeyCollision
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
// ViewConflict describes a view definition that diverged on both branches of a
// merge. Base is the common-ancestor definition (nil if the view did not exist
// there); Ours and Theirs are the two sides (nil if that side deleted the view).
type ViewConflict struct {
	Name          string
	ConflictID    string
	Base          *ViewDefinition
	Ours          *ViewDefinition
	Theirs        *ViewDefinition
	OurDiffType   string // "added", "modified", "deleted"
	TheirDiffType string // "added", "modified", "deleted"
}

// CollectionMetadata is the user-facing per-collection metadata, never the
// internal catalog representation.
type CollectionMetadata struct {
	Validator        *types.Document
	ValidationLevel  string
	ValidationAction string
}

// MetaConflict describes a collection whose durable metadata (validator/options)
// diverged on both branches of a merge. Base is the common-ancestor metadata
// (nil if absent there); Ours and Theirs are the two sides (nil if that side
// deleted the collection).
type MetaConflict struct {
	Collection    string
	ConflictID    string
	Base          *CollectionMetadata
	Ours          *CollectionMetadata
	Theirs        *CollectionMetadata
	OurDiffType   string // "added", "modified", "deleted"
	TheirDiffType string
	Reason        ConflictReason
}

type ConflictsResult struct {
	Collections []CollectionConflicts
	Views       []ViewConflict
	Metadata    []MetaConflict
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
	ConnBranch string // branch name from the connection's encoded db name (used for HEAD -> decoration)
	Limit      int32
	From       []string // optional: seed commit hashes for the traversal frontier; empty means start at HEAD
	Stat       bool     // when true, include per-collection change counts for each commit
	Patch      bool     // when true, include full document-level diffs for each commit
	All        bool     // when true, seed the walk with every branch HEAD (mutually exclusive with From)

	// Filters restricts the log to commits that touched (added/removed/modified
	// vs parent1) a matching document in any listed collection (OR across
	// collections and across each collection's matchers). Nil/empty map means no
	// filtering. When set, Stat/Patch output is scoped to the matched documents.
	Filters map[string]CommitFilter
}

// CommitFilter selects documents within one collection for the dumboLog filter.
// All, IDs, and Queries OR together.
type CommitFilter struct {
	// All matches any document in the collection (whole-collection wildcard).
	All bool
	// IDs are explicit _id values to match (any valid BSON _id type).
	IDs []any
	// Queries are find()-style predicates ($match), each resolved ONCE against
	// the collection at the connection branch's HEAD into a set of _ids, then
	// matched by identity. A query that resolves to no _ids matches nothing
	// (distinct from All).
	Queries []*types.Document
}

// CommitInfo represents a single commit entry returned by DumboDBLog.
type CommitInfo struct {
	CommitID           string
	Parent1            string // empty for root commit (no parent)
	Parent2            string // non-empty only for merge commits
	Author             string
	Email              string
	Message            string
	Timestamp          int64            // Unix milliseconds (author date)
	Committer          string           // "Name <email>" of the committer; equals Author when not explicitly set
	CommitterTimestamp int64            // Unix milliseconds (committer date)
	Refs               []string         // branch/tag decorations; empty when commit is not a branch head
	Stat               []TableStatus    // per-collection change summary (only when LogParams.Stat is true)
	Diff               []CollectionDiff // full document diffs (only when LogParams.Patch is true)
	ViewStat           []ViewStatus     // per-view change summary (only when LogParams.Stat is true)
	ViewDiff           []ViewChange     // full view definition diffs (only when LogParams.Patch is true)
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

	// Path-based field diffs of the collection's validator/options, empty when
	// the metadata did not change. Same shape as ModifiedDoc.Diff.
	MetadataDiff []FieldDiff
}

type VersioningStatusResult struct {
	Branch    string
	CommitID  string // HEAD commit hash; populated only when the workspace is clean (no changes)
	ReadOnly  bool
	Tables    []TableStatus
	MergeOp   string            // "merge", "cherry-pick", "rebase", or "revert"; empty when no operation in progress
	Conflicts []ConflictSummary // per-collection conflict counts; empty when no conflicts
	Views     []ViewStatus
}

type ViewStatus struct {
	Name   string
	Status string // "added", "modified", or "deleted"
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

// FieldDiff represents a single field-level change, either within a modified
// document or within a collection's metadata.
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

	// Path-based field diffs of the collection's validator/options, empty when
	// the metadata did not change. A validator change reports the changed
	// leaves inside the validator, so paths reach into it (e.g.
	// "$.validator.$jsonSchema.properties.email.pattern").
	MetadataDiff []FieldDiff
}

type ViewDefinition struct {
	ViewOn   string
	Pipeline *types.Array
}

// ViewChange represents a view added, removed, or redefined between two
// revisions. From is nil for an added view; To is nil for a removed view; both
// are set for a redefine.
type ViewChange struct {
	Name   string
	Status string // "added", "deleted", or "modified"
	From   *ViewDefinition
	To     *ViewDefinition
}

// DiffResult represents the result of VersioningBackend.DumboDBDiff method.
// Only collections and views with at least one change appear.
type DiffResult struct {
	Collections []CollectionDiff
	Views       []ViewChange
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

// BranchStatusParams represents the parameters of VersioningBackend.DumboDBBranchStatus.
//
// Base and each entry of Targets are rootish expressions (commit hash, branch name,
// tag name, or ancestor expression like "main~2"). The backend reports, for each
// target, how many commits it is ahead and behind the base.
type BranchStatusParams struct {
	DBName  string
	Base    string
	Targets []string
}

// BranchStatusEntry is the ahead/behind result for a single target refspec.
//
// Target echoes the corresponding BranchStatusParams.Targets entry verbatim (e.g.
// "main~2"); the caller is responsible for any HEAD/HEAD~N rewriting before
// invoking the backend. Hash is the 32-character commit the refspec resolved to.
// CommitsAhead counts commits reachable from the target but not the base;
// CommitsBehind counts the reverse.
type BranchStatusEntry struct {
	Target        string
	Hash          string
	CommitsAhead  int32
	CommitsBehind int32
}

// BranchStatusResult represents the result of VersioningBackend.DumboDBBranchStatus.
// BaseTarget echoes the input base refspec; BaseHash is its resolved commit hash.
type BranchStatusResult struct {
	BaseTarget string
	BaseHash   string
	Entries    []BranchStatusEntry
}

// VersioningBackend is an optional interface for backends that support Dolt versioning operations.
// The handler checks for this interface via type assertion; backends that don't implement it
// will cause the dumbodb versioning commands to return an unsupported error.
type VersioningBackend interface {
	// DumboDBCommit commits the current working set on the given branch with the provided message.
	DumboDBCommit(context.Context, *CommitParams) (*CommitResult, error)

	// DumboDBBranch creates, deletes, or lists branches; the operation is selected
	// by the combination of BranchParams fields.
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

	// DumboDBRemote adds, lists, or removes a named remote for a database.
	DumboDBRemote(context.Context, *RemoteParams) (*RemoteResult, error)

	// DumboDBPush pushes a branch's committed HEAD to a configured remote.
	DumboDBPush(context.Context, *PushParams) (*PushResult, error)

	// DumboDBFetch fetches a branch from a remote and updates the tracking ref.
	DumboDBFetch(context.Context, *FetchParams) (*FetchResult, error)

	// DumboDBClone creates a new database by cloning a file:// remote.
	DumboDBClone(context.Context, *CloneParams) (*CloneResult, error)

	// DumboDBPull fetches from a remote and merges the fetched commit into the
	// current branch (git pull = fetch + merge).
	DumboDBPull(context.Context, *PullParams) (*PullResult, error)

	// DumboDBGC runs garbage collection on the database's chunk store.
	// Every branch in the database is in scope (one chunk store per
	// logical database). Mode is "default" (sweep new-gen / unreferenced
	// old-gen) or "full" (rewrite every chunk). Requires a session in
	// ctx; the calling session participates in the GC safepoint as the
	// callSession (excluded from the waited set).
	DumboDBGC(context.Context, *GCParams) (*GCResult, error)

	// DumboDBBranchStatus reports how many commits each target refspec is ahead and behind the base refspec.
	DumboDBBranchStatus(context.Context, *BranchStatusParams) (*BranchStatusResult, error)

	// UndropDatabase restores a copy of a soft-deleted database: the drop stays
	// preserved and listed so it can be restored again. Empty DropID restores the
	// most recent drop; ToDatabase restores under a different name (default Name).
	UndropDatabase(context.Context, *UndropParams) (*UndropResult, error)

	// ListDroppedDatabases returns every preserved drop, most recently dropped first.
	ListDroppedDatabases(context.Context) (*DroppedDatabasesResult, error)

	// PurgeDroppedDatabases permanently removes preserved drops matching the filter
	// (Name required), returning the removed drops.
	PurgeDroppedDatabases(context.Context, *PurgeDroppedParams) (*PurgeDroppedResult, error)
}

// PurgeDroppedParams filters PurgeDroppedDatabases; a drop must satisfy every set field.
type PurgeDroppedParams struct {
	Name          string    // required: exact database name
	DropID        string    // optional: exact drop id
	DroppedBefore time.Time // optional: only drops dropped strictly before this
}

type PurgeDroppedResult struct {
	Purged []DroppedDatabase
}

type UndropParams struct {
	Name       string
	DropID     string // optional: selects one drop when Name has several
	ToDatabase string // optional: restore under this name instead of Name
}

type UndropResult struct {
	Name   string
	DropID string
}

type DroppedDatabase struct {
	Name              string
	DropID            string // UnixNano of the drop; disambiguates repeat drops of one name
	DroppedAtUnixNano int64
}

type DroppedDatabasesResult struct {
	Databases []DroppedDatabase
}

// RemoteParams are the arguments to DumboDBRemote.
type RemoteParams struct {
	DBName string
	Action string // "add", "list", or "remove"
	Name   string // remote name (add, remove)
	URL    string // remote url (add)
}

// RemoteInfo describes a single configured remote.
type RemoteInfo struct {
	Name string
	URL  string
}

// RemoteResult is returned by DumboDBRemote. For list it holds all remotes for
// the database; for add it holds the created remote; for remove it is empty.
type RemoteResult struct {
	Remotes []RemoteInfo
}

// PushParams are the arguments to DumboDBPush.
type PushParams struct {
	DBName     string
	Remote     string // remote name; empty resolves from config.push/config.pull
	ConnBranch string // the connection's current branch; the local branch for a bare push and the target of HEAD
	RefSpec    string // git-style [+]<src>[:<dst>]; empty means a bare push of the connection branch (git push)
	Force      bool   // non-fast-forward (force) update; equivalent to a leading '+' in the refspec
}

// PushResult is returned by DumboDBPush.
type PushResult struct {
	Remote       string
	URL          string
	Branch       string // local branch pushed; empty when the source was a revision expression, not a branch
	RemoteBranch string // destination branch on the remote (equals Branch unless a refspec renamed it)
	CommitBefore string // remote branch head before the push; empty when the push created the branch
	CommitPushed string // commit now on the remote branch
	UpToDate     bool   // true when the remote already had the commit
}

// FetchParams are the arguments to DumboDBFetch.
type FetchParams struct {
	DBName string
	Remote string // remote name (looked up in admin.system.remotes)
}

// FetchedRef is one remote branch updated by a fetch.
type FetchedRef struct {
	Branch       string
	CommitBefore string // local tracking-ref head before the fetch; empty when the ref is new
	Commit       string // tracking-ref head after the fetch
}

// FetchResult is returned by DumboDBFetch. Like git fetch, every remote branch
// is pulled into a local tracking ref refs/remotes/<remote>/<branch>.
type FetchResult struct {
	Remote   string
	URL      string
	Branches []FetchedRef
}

// PullParams are the arguments to DumboDBPull.
type PullParams struct {
	DBName    string
	Branch    string // current branch to pull into (the connection branch)
	Remote    string // remote to pull from; empty means the branch upstream
	NoFF      bool   // force a merge commit even when a fast-forward is possible
	FFOnly    bool   // fail if the pull is not a fast-forward
	FFSet     bool   // whether NoFF/FFOnly were passed explicitly
	Rebase    string // rebase onto the fetched commit instead of merging
	RebaseSet bool   // whether Rebase was passed explicitly
	Message   string // optional merge commit message
	Author    string // optional 'Name <email>' for a merge commit
}

// PullResult is returned by DumboDBPull.
type PullResult struct {
	Remote          string
	Branch          string
	CommitBefore    string // local branch head before the pull
	CommitAfter     string // local branch head after the pull
	FastForward     bool   // the pull advanced the branch without a merge commit
	AlreadyUpToDate bool   // the branch already had the fetched commit
	Rebased         bool   // the pull rebased instead of merging
}

// CloneParams are the arguments to DumboDBClone.
type CloneParams struct {
	From string // remote url (file:// only for now)
	As   string // new database name
	// TrackAsMain maps this remote branch onto the clone's local main (for a
	// remote whose default is not main); empty means require a remote "main".
	TrackAsMain string
}

// CloneResult is returned by DumboDBClone.
type CloneResult struct {
	DB  string
	URL string
}
