/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// testSnapshotCloneDatabase is the shared clone-database name used across
// snapshot-recovery tests that need a non-empty CloneDatabase.
const testSnapshotCloneDatabase = "snapshot_1"

type vcAPITestEnv struct {
	api           *validatorCommitterAPI
	mockVcService *mock.VcService
}

func newVCAPITestEnv(t *testing.T, numVCService int) *vcAPITestEnv {
	t.Helper()
	vcs, servers := mock.StartMockVCService(t, test.StartServerParameters{NumService: numVCService})
	api, err := newValidatorCommitterAPI(
		test.ServerToMultiClientConfig(test.InsecureTLSConfig, servers.Configs...), newPolicyManager(),
	)
	require.NoError(t, err)
	t.Cleanup(api.close)
	return &vcAPITestEnv{api: api, mockVcService: vcs}
}

func TestValidatorCommitterAPIGetLatestSnapshotState(t *testing.T) {
	t.Parallel()
	env := newVCAPITestEnv(t, 1)

	// No snapshot ever accepted: the mock's default zero-value SnapshotState is
	// returned (empty TxRef), matching the VC's "no prior snapshot" case.
	got, err := env.api.getLatestSnapshotState(t.Context())
	require.NoError(t, err)
	require.Nil(t, got.TxRef)

	// The mock returns whatever SnapshotState is injected via SetLatestSnapshotState.
	want := &committerpb.SnapshotState{
		TxRef: &committerpb.TxRef{TxId: "snap-api-1"}, Status: committerpb.SnapshotState_PENDING,
		CloneDatabase: testSnapshotCloneDatabase,
	}
	env.mockVcService.SetLatestSnapshotState(want)

	got, err = env.api.getLatestSnapshotState(t.Context())
	require.NoError(t, err)
	test.RequireProtoEqual(t, want, got)
}

func TestValidatorCommitterAPIRestartSnapshotHash(t *testing.T) {
	t.Parallel()
	env := newVCAPITestEnv(t, 1)

	require.NoError(t, env.api.restartSnapshotHash(t.Context(), "snap-api-restart-1"))
	require.Equal(t, []string{"snap-api-restart-1"}, env.mockVcService.RestartSnapshotHashCalls())
}
