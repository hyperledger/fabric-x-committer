/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testapp"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

type vcMgrTestEnv struct {
	validatorCommitterManager *validatorCommitterManager
	api                       *validatorCommitterAPI
	inputTxs                  chan dependencygraph.TxNodeBatch
	outputTxs                 chan dependencygraph.TxNodeBatch
	outputTxsStatus           *txStatusQueue
	mockVcService             *mock.VcService
	mockVCGrpcServers         *test.Servers
	sigVerTestEnv             *svMgrTestEnv
}

func newVcMgrTestEnv(t *testing.T, numVCService int) *vcMgrTestEnv {
	t.Helper()
	vcs, servers := mock.StartMockVCService(t, test.StartServerParameters{NumService: numVCService})
	svEnv := newSvMgrTestEnv(t, 2)

	inputTxs := make(chan dependencygraph.TxNodeBatch, 10)
	outputTxs := make(chan dependencygraph.TxNodeBatch, 10)
	outputTxsStatus := newTxStatusQueue(10)

	metrics := newPerformanceMetrics(&channels{
		sigVerifierToVCServiceValidatedTxs: inputTxs,
		vcServiceToDepGraphValidatedTxs:    outputTxs,
		vcServiceToCoordinatorTxStatus:     outputTxsStatus,
	})
	vcm := newValidatorCommitterManager(
		&validatorCommitterManagerConfig{
			clientConfig:                   test.ServerToMultiClientConfig(test.InsecureTLSConfig, servers.Configs...),
			incomingTxsForValidationCommit: inputTxs,
			outgoingValidatedTxsNode:       outputTxs,
			outgoingTxsStatus:              outputTxsStatus,
			metrics:                        metrics,
			policyMgr:                      svEnv.policyManager,
		},
	)

	api, err := newValidatorCommitterAPI(
		test.ServerToMultiClientConfig(test.InsecureTLSConfig, servers.Configs...), svEnv.policyManager,
	)
	require.NoError(t, err)
	t.Cleanup(api.close)

	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		err := connection.FilterStreamRPCError(vcm.run(ctx, api))
		assert.NoError(t, err)
		return nil
	}, nil)
	// Waiting for all the connections ensures the validatorCommitter slice is fully populated,
	// as newSvMgrTestEnv does for the verifiers.
	monitoring.WaitForConnections(
		t, metrics.Provider, "coordinator_vcservice_connection_status", numVCService,
	)

	return &vcMgrTestEnv{
		validatorCommitterManager: vcm,
		api:                       api,
		inputTxs:                  inputTxs,
		outputTxs:                 outputTxs,
		outputTxsStatus:           outputTxsStatus,
		mockVcService:             vcs,
		mockVCGrpcServers:         servers,
		sigVerTestEnv:             svEnv,
	}
}

func (e *vcMgrTestEnv) requireConnectionMetrics(
	t *testing.T,
	vcIndex, expectedConnStatus, expectedConnFailureTotal int,
) {
	t.Helper()
	require.Less(t, vcIndex, len(e.validatorCommitterManager.validatorCommitter))
	sv := e.validatorCommitterManager.validatorCommitter[vcIndex]
	monitoring.RequireConnectionMetrics(
		t, sv.conn.CanonicalTarget(),
		sv.metrics.vcs.connection,
		monitoring.ExpectedConn{Status: expectedConnStatus, FailureTotal: expectedConnFailureTotal},
	)
}

func (e *vcMgrTestEnv) requireRetriedTxsTotal(t *testing.T, expectedRetriedTxsTotal int) {
	t.Helper()
	test.EventuallyIntMetric(
		t, expectedRetriedTxsTotal, e.validatorCommitterManager.config.metrics.vcs.retriedTotal,
		5*time.Second, 250*time.Millisecond,
	)
}

func TestValidatorCommitterManagerX(t *testing.T) {
	t.Parallel()

	ensureZeroWaitingTxs := func(env *vcMgrTestEnv) {
		for _, vc := range env.validatorCommitterManager.validatorCommitter {
			require.Zero(t, vc.txBeingValidated.Count())
		}
	}

	t.Run("Send tx batch to use any vcservice and send a batch with larger size", func(t *testing.T) {
		t.Parallel()
		env := newVcMgrTestEnv(t, 2)
		txBatch, expectedTxsStatus := createInputTxsNodeForTest(t, 5, 0, 1)
		env.inputTxs <- txBatch

		outTxs := <-env.outputTxs
		require.ElementsMatch(t, txBatch, outTxs)

		outTxsStatus := env.readOutputTxsStatus(t)

		test.RequireProtoElementsMatch(t, expectedTxsStatus, outTxsStatus.Status)

		test.EventuallyIntMetric(
			t, 5, env.validatorCommitterManager.config.metrics.vcs.processedTotal,
			2*time.Second, 100*time.Millisecond,
		)

		totalBlocks := 3
		txPerBlock := 50
		txBatches := make(dependencygraph.TxNodeBatch, 0, totalBlocks*txPerBlock)
		expectedTxsStatus = make([]*committerpb.TxStatus, 0, totalBlocks*txPerBlock)

		for i := range totalBlocks {
			//nolint:gosec // int -> int64
			curTxBatch, txStatus := createInputTxsNodeForTest(t, txPerBlock, 1024*1024, uint64(i+2))
			txBatches = append(txBatches, curTxBatch...)
			expectedTxsStatus = append(expectedTxsStatus, txStatus...)
		}

		env.inputTxs <- txBatches

		// txBatch would be split into three parts, one per block.
		outTxs = <-env.outputTxs
		outTxs = append(outTxs, <-env.outputTxs...)
		outTxs = append(outTxs, <-env.outputTxs...)
		require.ElementsMatch(t, txBatches, outTxs)

		outTxsStatus = env.readOutputTxsStatus(t)
		status := env.readOutputTxsStatus(t)
		outTxsStatus.Status = append(outTxsStatus.Status, status.Status...)
		status = env.readOutputTxsStatus(t)
		outTxsStatus.Status = append(outTxsStatus.Status, status.Status...)
		test.RequireProtoElementsMatch(t, expectedTxsStatus, outTxsStatus.Status)

		test.EventuallyIntMetric(
			t, 5+totalBlocks*txPerBlock,
			env.validatorCommitterManager.config.metrics.vcs.processedTotal,
			2*time.Second, 100*time.Millisecond,
		)

		ensureZeroWaitingTxs(env)
	})

	t.Run("an empty batch is not sent to the vcservice", func(t *testing.T) {
		t.Parallel()
		env := newVcMgrTestEnv(t, 1)

		// The first batch of a stream goes through splitAndSendToVC, which already sends nothing
		// for an empty batch, so send a real batch first to get past that path.
		first, firstStatus := createInputTxsNodeForTest(t, 2, 0, 1)
		env.inputTxs <- first
		require.ElementsMatch(t, first, <-env.outputTxs)
		test.RequireProtoElementsMatch(t, firstStatus, env.readOutputTxsStatus(t).Status)
		require.Equal(t, uint32(1), env.mockVcService.NumBatchesReceived.Load())

		// An empty batch reaches the manager when every status in a verifier response was
		// untracked. It must not be marshalled and sent as an empty VcBatch.
		env.inputTxs <- dependencygraph.TxNodeBatch{}

		// A real batch behind it proves the empty one was skipped rather than merely delayed:
		// had it been sent, it would have been counted before this one.
		second, secondStatus := createInputTxsNodeForTest(t, 3, 0, 2)
		env.inputTxs <- second
		require.ElementsMatch(t, second, <-env.outputTxs)
		test.RequireProtoElementsMatch(t, secondStatus, env.readOutputTxsStatus(t).Status)

		require.Equal(t, uint32(2), env.mockVcService.NumBatchesReceived.Load())
		ensureZeroWaitingTxs(env)
	})

	t.Run("send batches to ensure all vcservices are used", func(t *testing.T) {
		t.Parallel()
		env := newVcMgrTestEnv(t, 2)

		txBatch1, expectedTxsStatus1 := createInputTxsNodeForTest(t, 5, 0, 2)
		txBatch2, expectedTxsStatus2 := createInputTxsNodeForTest(t, 5, 0, 3)

		require.Eventually(t, func() bool {
			env.inputTxs <- txBatch1
			env.inputTxs <- txBatch2

			outputTxBatch1 := <-env.outputTxs
			outputTxBatch2 := <-env.outputTxs

			outTxsStatus1 := env.readOutputTxsStatus(t)
			outTxsStatus2 := env.readOutputTxsStatus(t)

			require.ElementsMatch(
				t,
				append(txBatch1, txBatch2...),
				append(outputTxBatch1, outputTxBatch2...),
			)

			test.RequireProtoElementsMatch(
				t,
				append(expectedTxsStatus1, expectedTxsStatus2...),
				append(outTxsStatus1.Status, outTxsStatus2.Status...),
			)

			return env.mockVcService.NumBatchesReceived.Load() != 0
		}, 4*time.Second, 100*time.Millisecond)
		ensureZeroWaitingTxs(env)
	})

	t.Run("namespace transaction should update signature verifier", func(t *testing.T) {
		t.Parallel()
		env := newVcMgrTestEnv(t, 2)
		verifierStreams := mock.RequireStreams(t, env.sigVerTestEnv.mockVerifier, 2)
		for _, mockSvService := range verifierStreams {
			require.Empty(t, *mockSvService.Updates.Load())
		}

		_, verificationKey := testsig.NewKeyPair(signature.Ecdsa)
		p := policy.MakeECDSAThresholdRuleNsPolicy(verificationKey)
		pBytes, err := proto.Marshal(p)
		require.NoError(t, err)

		configBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(t.TempDir(), &testcrypto.ConfigBlock{})
		require.NoError(t, err)

		txBatch := []*dependencygraph.TransactionNode{
			{
				VCTx: &servicepb.VcTx{
					Ref: committerpb.NewTxRef("create config", 100, 63),
					Namespaces: []*applicationpb.TxNamespace{{
						NsId: committerpb.ConfigNamespaceID,
						BlindWrites: []*applicationpb.Write{{
							Key:   []byte(committerpb.ConfigKey),
							Value: configBlock.Data.Data[0],
						}},
					}},
				},
			},
			{
				VCTx: &servicepb.VcTx{
					Ref: committerpb.NewTxRef("create ns 1", 100, 64),
					Namespaces: []*applicationpb.TxNamespace{{
						NsId: committerpb.MetaNamespaceID,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   []byte("1"),
							Value: pBytes,
						}},
					}},
				},
			},
		}
		env.inputTxs <- txBatch

		outTxsStatus := env.readOutputTxsStatus(t)

		require.Len(t, outTxsStatus.Status, 2)
		expectedConfig := committerpb.NewTxStatus(committerpb.Status_COMMITTED, "create config", 100, 63)
		testapp.RequireStatus(t, expectedConfig, outTxsStatus.Status)
		expectedMeta := committerpb.NewTxStatus(committerpb.Status_COMMITTED, "create ns 1", 100, 64)
		testapp.RequireStatus(t, expectedMeta, outTxsStatus.Status)

		require.ElementsMatch(t, txBatch, <-env.outputTxs)

		expectedUpdate := &servicepb.VerifierUpdates{
			Config: &applicationpb.ConfigTransaction{
				Envelope: configBlock.Data.Data[0],
			},
			NamespacePolicies: &applicationpb.NamespacePolicies{
				Policies: []*applicationpb.PolicyItem{
					{
						Namespace: "1",
						Policy:    protoutil.MarshalOrPanic(p),
					},
				},
			},
		}
		update, _ := env.sigVerTestEnv.policyManager.getAll()
		requireUpdateEqual(t, expectedUpdate, update)
		ensureZeroWaitingTxs(env)
	})
}

func TestValidatorCommitterManagerRecovery(t *testing.T) {
	t.Parallel()
	env := newVcMgrTestEnv(t, 1)
	env.mockVcService.MockFaultyNodeDropSize = 4

	env.requireConnectionMetrics(t, 0, connection.Connected, 0)
	env.requireRetriedTxsTotal(t, 0)

	numTxs := 10
	txBatch, expectedTxsStatus := createInputTxsNodeForTest(t, numTxs, 0, 0)
	env.inputTxs <- txBatch

	require.Eventually(t, func() bool {
		count := env.validatorCommitterManager.validatorCommitter[0].txBeingValidated.Count()
		return count == numTxs-6
	}, 4*time.Second, 100*time.Millisecond)

	env.mockVCGrpcServers.ServersStop[0]()
	test.CheckServerStopped(t, env.mockVCGrpcServers.Configs[0].GRPC.Endpoint.Address())
	env.requireConnectionMetrics(t, 0, connection.Disconnected, 1)

	env.mockVcService.MockFaultyNodeDropSize = 0
	env.mockVCGrpcServers = mock.StartMockVCServiceFromServerConfig(
		t,
		env.mockVcService,
		env.mockVCGrpcServers.Configs...,
	)
	env.requireConnectionMetrics(t, 0, connection.Connected, 1)
	env.requireRetriedTxsTotal(t, 4)

	actualTxsStatus := make([]*committerpb.TxStatus, 0, numTxs)
	for range 2 {
		result := env.readOutputTxsStatus(t)
		actualTxsStatus = append(actualTxsStatus, result.Status...)
	}
	test.RequireProtoElementsMatch(t, expectedTxsStatus, actualTxsStatus)

	txProcessedTotalMetric := env.validatorCommitterManager.config.metrics.vcs.processedTotal
	txTotal := test.GetIntMetricValue(t, txProcessedTotalMetric)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	err := env.mockVcService.SubmitTransactions(ctx, &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{Ref: committerpb.NewTxRef("untrackedTxID1", 1, 1)},
			{Ref: committerpb.NewTxRef("untrackedTxID2", 2, 2)},
		},
	})
	require.NoError(t, err)

	require.Never(t, func() bool {
		return test.GetIntMetricValue(t, txProcessedTotalMetric) > txTotal
	}, 2*time.Second, 1*time.Second)
}

// TestValidatorCommitterAddAndRecoverPendingTxs covers the tracking invariant that
// receiveStatusAndForwardToOutput depends on when forwarding a result fails: every transaction
// returned to txBeingValidated must be recoverable.
func TestValidatorCommitterAddAndRecoverPendingTxs(t *testing.T) {
	t.Parallel()
	// The connection is unused: neither call below touches the stream.
	vc := newValidatorCommitter(nil, newTestPerfMetrics(), newPolicyManager())

	txsNode := dependencygraph.TxNodeBatch{
		{VCTx: &servicepb.VcTx{Ref: committerpb.NewTxRef("tx 1", 1, 0)}},
		{VCTx: &servicepb.VcTx{Ref: committerpb.NewTxRef("tx 2", 2, 0)}},
		{VCTx: &servicepb.VcTx{Ref: committerpb.NewTxRef("tx 3", 1, 1)}},
	}
	vc.addTxsBeingValidated(txsNode)
	require.Equal(t, len(txsNode), vc.txBeingValidated.Count())

	recovered := make(chan dependencygraph.TxNodeBatch, 1)
	vc.recoverPendingTransactions(channel.NewWriter(t.Context(), recovered))

	require.ElementsMatch(t, txsNode, <-recovered)
	require.Zero(t, vc.txBeingValidated.Count())
	test.RequireIntMetricValue(t, len(txsNode), vc.metrics.vcs.retriedTotal)
}

func TestSweepSnapshotRecoveryNoSnapshotEverAccepted(t *testing.T) {
	t.Parallel()
	env := newVcMgrTestEnv(t, 2)

	// No snapshot ever accepted: no RestartSnapshotHash call on any VC.
	require.NoError(t, env.validatorCommitterManager.sweepSnapshotRecovery(t.Context(), env.api))
	require.Empty(t, env.mockVcService.RestartSnapshotHashCalls())
}

func TestSweepSnapshotRecoveryTerminalStatusIsNoOp(t *testing.T) {
	t.Parallel()
	// Success cases: COMPLETED and CHECKPOINTED never trigger a restart call.
	for _, tc := range []struct {
		name   string
		status committerpb.SnapshotState_Status
	}{
		{name: "COMPLETED", status: committerpb.SnapshotState_COMPLETED},
		{name: "CHECKPOINTED", status: committerpb.SnapshotState_CHECKPOINTED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newVcMgrTestEnv(t, 2)
			env.mockVcService.SetLatestSnapshotState(&committerpb.SnapshotState{
				TxRef: &committerpb.TxRef{TxId: "snap-sweep-terminal"}, Status: tc.status,
			})

			require.NoError(t, env.validatorCommitterManager.sweepSnapshotRecovery(t.Context(), env.api))
			require.Empty(t, env.mockVcService.RestartSnapshotHashCalls())
		})
	}
}

func TestSweepSnapshotRecoveryClaimedByLiveVCIsNoOp(t *testing.T) {
	t.Parallel()
	env := newVcMgrTestEnv(t, 2)
	txID := "snap-sweep-claimed"
	env.mockVcService.SetLatestSnapshotState(&committerpb.SnapshotState{
		TxRef: &committerpb.TxRef{TxId: txID}, Status: committerpb.SnapshotState_IN_PROGRESS,
		CloneDatabase: testSnapshotCloneDatabase,
	})
	env.mockVcService.SetOwnsSnapshotHashJob(txID, true)

	require.NoError(t, env.validatorCommitterManager.sweepSnapshotRecovery(t.Context(), env.api))
	require.Empty(t, env.mockVcService.RestartSnapshotHashCalls())
}

func TestSweepSnapshotRecoveryUnclaimedCallsRestartHash(t *testing.T) {
	t.Parallel()
	// Success cases: PENDING/IN_PROGRESS/FAILED with no live owner all call
	// RestartSnapshotHash exactly once.
	for _, tc := range []struct {
		name   string
		status committerpb.SnapshotState_Status
	}{
		{name: "PENDING", status: committerpb.SnapshotState_PENDING},
		{name: "IN_PROGRESS", status: committerpb.SnapshotState_IN_PROGRESS},
		{name: "FAILED", status: committerpb.SnapshotState_FAILED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newVcMgrTestEnv(t, 2)
			txID := "snap-sweep-unclaimed-" + tc.name
			env.mockVcService.SetLatestSnapshotState(&committerpb.SnapshotState{
				TxRef: &committerpb.TxRef{TxId: txID}, Status: tc.status, CloneDatabase: testSnapshotCloneDatabase,
			})
			// No SetOwnsSnapshotHashJob call: every VC reports unowned by default.

			require.NoError(t, env.validatorCommitterManager.sweepSnapshotRecovery(t.Context(), env.api))
			require.Equal(t, []string{txID}, env.mockVcService.RestartSnapshotHashCalls())
		})
	}
}

// TestAnyVCOwnsSnapshotHashJobWaitsForValidatorCommitterReady is the
// regression test for the startup race this design fixed: before run has
// been called at all, vcm.validatorCommitter is nil, and without
// validatorCommitterReady gating the read, anyVCOwnsSnapshotHashJob would
// wrongly and immediately report "not owned" -- indistinguishable from every
// VC genuinely not owning the job -- even though no VC was actually asked.
// With the gate in place, the call must instead block until run signals
// readiness (here, via ctx ending first, proving it actually waited rather
// than racing through on the nil slice).
func TestAnyVCOwnsSnapshotHashJobWaitsForValidatorCommitterReady(t *testing.T) {
	t.Parallel()
	vcm := newValidatorCommitterManager(&validatorCommitterManagerConfig{})
	require.Nil(t, vcm.validatorCommitter)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	// run was never called, so validatorCommitterReady never fires: the call
	// must block until ctx ends, not return a false "unowned" immediately.
	start := time.Now()
	owned, err := vcm.anyVCOwnsSnapshotHashJob(ctx, "snap-race-regression")
	require.False(t, owned)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
}

func (e *vcMgrTestEnv) readOutputTxsStatus(t *testing.T) *committerpb.TxStatusBatch {
	t.Helper()
	batch, ok := e.outputTxsStatus.read(t.Context())
	require.True(t, ok)
	return batch
}

func createInputTxsNodeForTest(t *testing.T, numTxs, valueSize int, blkNum uint64) (
	[]*dependencygraph.TransactionNode, []*committerpb.TxStatus,
) {
	t.Helper()

	txsNode := make([]*dependencygraph.TransactionNode, numTxs)
	expectedTxsStatus := make([]*committerpb.TxStatus, numTxs)

	for i := range numTxs {
		id := uuid.NewString()
		txsNode[i] = &dependencygraph.TransactionNode{
			VCTx: &servicepb.VcTx{
				Ref: committerpb.NewTxRef(id, blkNum, uint32(i)), //nolint:gosec
				Namespaces: []*applicationpb.TxNamespace{{
					BlindWrites: []*applicationpb.Write{{
						Value: utils.MustRead(rand.Reader, valueSize),
					}},
				}},
			},
		}
		//nolint:gosec // int -> uint32.
		expectedTxsStatus[i] = committerpb.NewTxStatus(committerpb.Status_COMMITTED, id, blkNum, uint32(i))
	}

	return txsNode, expectedTxsStatus
}
