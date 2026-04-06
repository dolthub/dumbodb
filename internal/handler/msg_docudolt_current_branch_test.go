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

package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/docudolt/internal/backends"
	"github.com/dolthub/docudolt/internal/handler/common"
	"github.com/dolthub/docudolt/internal/handler/handlererrors"
	"github.com/dolthub/docudolt/internal/types"
	"github.com/dolthub/docudolt/internal/util/must"
)

// versioningBackendMock is a minimal Backend + VersioningBackend implementation
// for handler unit tests. DocuDoltCurrentBranch echoes params.Branch; all other
// methods are no-ops that satisfy the interface.
type versioningBackendMock struct{}

// backends.Backend methods.

func (m *versioningBackendMock) Close() {}
func (m *versioningBackendMock) Status(_ context.Context, _ *backends.StatusParams) (*backends.StatusResult, error) {
	return &backends.StatusResult{}, nil
}
func (m *versioningBackendMock) Database(_ string) (backends.Database, error)   { return nil, nil }
func (m *versioningBackendMock) Describe(_ chan<- *prometheus.Desc)              {}
func (m *versioningBackendMock) Collect(_ chan<- prometheus.Metric)              {}
func (m *versioningBackendMock) ListDatabases(_ context.Context, _ *backends.ListDatabasesParams) (*backends.ListDatabasesResult, error) {
	return &backends.ListDatabasesResult{}, nil
}
func (m *versioningBackendMock) DropDatabase(_ context.Context, _ *backends.DropDatabaseParams) error {
	return nil
}

// backends.VersioningBackend methods.

func (m *versioningBackendMock) DocuDoltCurrentBranch(_ context.Context, p *backends.CurrentBranchParams) (*backends.CurrentBranchResult, error) {
	return &backends.CurrentBranchResult{Branch: p.Branch}, nil
}
func (m *versioningBackendMock) DocuDoltCommit(_ context.Context, _ *backends.CommitParams) (*backends.CommitResult, error) {
	return nil, nil
}
func (m *versioningBackendMock) DocuDoltBranch(_ context.Context, _ *backends.BranchParams) (*backends.BranchResult, error) {
	return nil, nil
}
func (m *versioningBackendMock) DocuDoltMerge(_ context.Context, _ *backends.MergeParams) (*backends.MergeResult, error) {
	return nil, nil
}
func (m *versioningBackendMock) DocuDoltLog(_ context.Context, _ *backends.LogParams) (*backends.LogResult, error) {
	return nil, nil
}
func (m *versioningBackendMock) DocuDoltStatus(_ context.Context, _ *backends.VersioningStatusParams) (*backends.VersioningStatusResult, error) {
	return nil, nil
}
func (m *versioningBackendMock) DocuDoltDiff(_ context.Context, _ *backends.DiffParams) (*backends.DiffResult, error) {
	return nil, nil
}
func (m *versioningBackendMock) DocuDoltReset(_ context.Context, _ *backends.ResetParams) (*backends.ResetResult, error) {
	return nil, nil
}
func (m *versioningBackendMock) DocuDoltConflicts(_ context.Context, _ *backends.ConflictsParams) (*backends.ConflictsResult, error) {
	return nil, nil
}
func (m *versioningBackendMock) DocuDoltResolveConflict(_ context.Context, _ *backends.ResolveConflictParams) (*backends.ResolveConflictResult, error) {
	return nil, nil
}
func (m *versioningBackendMock) DocuDoltCherryPick(_ context.Context, _ *backends.CherryPickParams) (*backends.CherryPickResult, error) {
	return nil, nil
}

// makeCurrentBranchMsg creates a wire.OpMsg for docuDoltCurrentBranch with the given encoded $db.
func makeCurrentBranchMsg(encodedDB string) *wire.OpMsg {
	doc := must.NotFail(types.NewDocument("doltCurrentBranch", int32(1), "$db", encodedDB))
	return must.NotFail(documentOpMsg(doc))
}

// TestMsgDocuDoltCurrentBranch_ReadOnly verifies that docuDoltCurrentBranch returns
// ErrOperationFailed for read-only rootishes (commit hashes and ancestor expressions).
// The error message must mention "no current branch name" to be actionable.
func TestMsgDocuDoltCurrentBranch_ReadOnly(t *testing.T) {
	t.Parallel()

	// No backend needed: the handler rejects read-only rootishes before ever
	// consulting the backend.
	h := &Handler{}

	cases := []struct {
		name      string
		encodedDB string
	}{
		// Exactly 32 lowercase base32 chars → Dolt commit hash → read-only.
		{"commit_hash", "mydb__d_na7kfra98h45fr2u5qtr30o2ggm7vh61"},
		{"all_zeros_hash", "mydb__d_00000000000000000000000000000000"},
		// <branch>~<N> ancestor expression → read-only.
		{"ancestor_tilde1", "mydb__d_main~1"},
		{"ancestor_tilde3", "mydb__d_feature~3"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			msg := makeCurrentBranchMsg(tc.encodedDB)
			_, err := h.MsgDocuDoltCurrentBranch(context.Background(), msg)

			require.Error(t, err)

			var cmdErr *handlererrors.CommandError
			require.True(t, errors.As(err, &cmdErr), "expected *CommandError, got %T: %v", err, err)
			assert.Equal(t, handlererrors.ErrOperationFailed, cmdErr.Code(),
				"read-only rootish must produce ErrOperationFailed")
			assert.Contains(t, cmdErr.Error(), "no current branch name",
				"error message must mention no current branch name so callers know what to do")
		})
	}
}

// TestMsgDocuDoltCurrentBranch_Branch verifies that docuDoltCurrentBranch returns the
// correct branch name for writable connections (branch names and tag-like strings).
//
// Tags are syntactically indistinguishable from branch names at parse time, so
// they are treated as writable and the tag name is returned as the branch identifier.
func TestMsgDocuDoltCurrentBranch_Branch(t *testing.T) {
	t.Parallel()

	h := &Handler{b: &versioningBackendMock{}}

	cases := []struct {
		name       string
		encodedDB  string
		wantBranch string
	}{
		{"plain_db_name_defaults_to_main", "mydb", "main"},
		{"explicit_main", "mydb__d_main", "main"},
		{"feature_branch", "mydb__d_feature-x", "feature-x"},
		// Tag-like names (e.g. "v1.0") are indistinguishable from branch names at
		// parse time; docuDoltCurrentBranch returns the tag name as the branch.
		{"tag_like_v1_0", "mydb__d_v1.0", "v1.0"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			msg := makeCurrentBranchMsg(tc.encodedDB)
			resp, err := h.MsgDocuDoltCurrentBranch(context.Background(), msg)
			require.NoError(t, err)

			doc, err := opMsgDocument(resp)
			require.NoError(t, err)

			branch, err := common.GetRequiredParam[string](doc, "branch")
			require.NoError(t, err)
			assert.Equal(t, tc.wantBranch, branch)
		})
	}
}
