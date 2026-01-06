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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/apptest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type vcMgrTestEnv struct {
	validatorCommitterManager *validatorCommitterManager
	inputTxs                  chan dependencygraph.TxNodeBatch
	outputTxs                 chan dependencygraph.TxNodeBatch
	outputTxsStatus           chan *committerpb.TxStatusBatch
	mockVcService             *mock.VcService
	mockVCGrpcServers         *test.GrpcServers
	sigVerTestEnv             *svMgrTestEnv
}

func newVcMgrTestEnv(t *testing.T, numVCService int) *vcMgrTestEnv {
	t.Helper()
	vcs, servers := mock.StartMockVCService(t, numVCService)
	svEnv := newSvMgrTestEnv(t, 2)

	inputTxs := make(chan dependencygraph.TxNodeBatch, 10)
	outputTxs := make(chan dependencygraph.TxNodeBatch, 10)
	outputTxsStatus := make(chan *committerpb.TxStatusBatch, 10)

	vcm := newValidatorCommitterManager(
		&validatorCommitterManagerConfig{
			clientConfig:                   test.ServerToMultiClientConfig(servers.Configs...),
			incomingTxsForValidationCommit: inputTxs,
			outgoingValidatedTxsNode:       outputTxs,
			outgoingTxsStatus:              outputTxsStatus,
			metrics:                        newPerformanceMetrics(),
			policyMgr:                      svEnv.policyManager,
		},
	)

	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		err := connection.FilterStreamRPCError(vcm.run(ctx))
		assert.NoError(t, err)
		return nil
	}, vcm.ready.WaitForReady)

	return &vcMgrTestEnv{
		validatorCommitterManager: vcm,
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
		sv.metrics.vcservicesConnection,
		monitoring.ExpectedConn{Status: expectedConnStatus, FailureTotal: expectedConnFailureTotal},
	)
}

func (e *vcMgrTestEnv) requireRetriedTxsTotal(t *testing.T, expectedRetriedTxsTotal int) {
	t.Helper()
	test.EventuallyIntMetric(
		t, expectedRetriedTxsTotal, e.validatorCommitterManager.config.metrics.vcservicesRetriedTransactionTotal,
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

		outTxsStatus := <-env.outputTxsStatus

		test.RequireProtoElementsMatch(t, expectedTxsStatus, outTxsStatus.Status)

		test.EventuallyIntMetric(
			t, 5, env.validatorCommitterManager.config.metrics.vcserviceTransactionProcessedTotal,
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

		outTxsStatus = <-env.outputTxsStatus
		status := <-env.outputTxsStatus
		outTxsStatus.Status = append(outTxsStatus.Status, status.Status...)
		status = <-env.outputTxsStatus
		outTxsStatus.Status = append(outTxsStatus.Status, status.Status...)
		test.RequireProtoElementsMatch(t, expectedTxsStatus, outTxsStatus.Status)

		test.EventuallyIntMetric(
			t, 5+totalBlocks*txPerBlock,
			env.validatorCommitterManager.config.metrics.vcserviceTransactionProcessedTotal,
			2*time.Second, 100*time.Millisecond,
		)

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

			outTxsStatus1 := <-env.outputTxsStatus
			outTxsStatus2 := <-env.outputTxsStatus

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

			return env.mockVcService.GetNumBatchesReceived() != 0
		}, 4*time.Second, 100*time.Millisecond)
		ensureZeroWaitingTxs(env)
	})

	t.Run("namespace transaction should update signature verifier", func(t *testing.T) {
		t.Parallel()
		env := newVcMgrTestEnv(t, 2)
		verifierStreams := requireStreams(t, env.sigVerTestEnv.mockVerifier, 2)
		for _, mockSvService := range verifierStreams {
			require.Empty(t, mockSvService.GetUpdates())
		}

		_, verificationKey := sigtest.NewKeyPair(signature.Ecdsa)
		p := policy.MakeECDSAThresholdRuleNsPolicy(verificationKey)
		pBytes, err := proto.Marshal(p)
		require.NoError(t, err)

		configBlock, err := workload.CreateDefaultConfigBlock(&workload.ConfigBlock{
			MetaNamespaceVerificationKey: verificationKey,
		})
		require.NoError(t, err)

		txBatch := []*dependencygraph.TransactionNode{
			{
				Tx: &servicepb.VcTx{
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
				Tx: &servicepb.VcTx{
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

		outTxsStatus := <-env.outputTxsStatus

		require.Len(t, outTxsStatus.Status, 2)
		expectedConfig := committerpb.NewTxStatus(committerpb.Status_COMMITTED, "create config", 100, 63)
		apptest.RequireStatus(t, expectedConfig, outTxsStatus.Status)
		expectedMeta := committerpb.NewTxStatus(committerpb.Status_COMMITTED, "create ns 1", 100, 64)
		apptest.RequireStatus(t, expectedMeta, outTxsStatus.Status)

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

	env.mockVCGrpcServers.Servers[0].Stop()
	test.CheckServerStopped(t, env.mockVCGrpcServers.Configs[0].Endpoint.Address())
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
		result := <-env.outputTxsStatus
		actualTxsStatus = append(actualTxsStatus, result.Status...)
	}
	test.RequireProtoElementsMatch(t, expectedTxsStatus, actualTxsStatus)

	txProcessedTotalMetric := env.validatorCommitterManager.config.metrics.vcserviceTransactionProcessedTotal
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

func createInputTxsNodeForTest(t *testing.T, numTxs, valueSize int, blkNum uint64) (
	[]*dependencygraph.TransactionNode, []*committerpb.TxStatus,
) {
	t.Helper()

	txsNode := make([]*dependencygraph.TransactionNode, numTxs)
	expectedTxsStatus := make([]*committerpb.TxStatus, numTxs)

	for i := range numTxs {
		id := uuid.NewString()
		txsNode[i] = &dependencygraph.TransactionNode{
			Tx: &servicepb.VcTx{
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
