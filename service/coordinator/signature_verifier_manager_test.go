/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"crypto/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type svMgrTestEnv struct {
	signVerifierManager *signatureVerifierManager
	inputTxBatch        chan dependencygraph.TxNodeBatch
	outputValidatedTxs  chan dependencygraph.TxNodeBatch
	mockVerifier        *mock.Verifier
	grpcServers         *test.GrpcServers
	policyManager       *policyManager
	curBlockNum         atomic.Uint64
}

func newSvMgrTestEnv(t *testing.T, numSvService int, expectedEndErrorMsg ...byte) *svMgrTestEnv {
	t.Helper()
	expectedEndError := string(expectedEndErrorMsg)
	verifier, sc := mock.StartMockVerifierService(t, numSvService)

	inputTxBatch := make(chan dependencygraph.TxNodeBatch, 10)
	outputValidatedTxs := make(chan dependencygraph.TxNodeBatch, 10)

	pm := newPolicyManager()
	svm := newSignatureVerifierManager(
		&signVerifierManagerConfig{
			clientConfig:             test.ServerToMultiClientConfig(sc.Configs...),
			incomingTxsForValidation: inputTxBatch,
			outgoingValidatedTxs:     outputValidatedTxs,
			metrics:                  newPerformanceMetrics(),
			policyManager:            pm,
		},
	)

	test.RunServiceForTest(t.Context(), t,
		func(ctx context.Context) error {
			err := connection.FilterStreamRPCError(svm.run(ctx))
			if expectedEndError != "" {
				require.ErrorContains(t, err, expectedEndError)
			} else {
				assert.NoError(t, err)
			}
			return nil
		},
		nil,
	)
	monitoring.WaitForConnections(
		t, svm.metrics.Provider, "coordinator_grpc_verifier_connection_status", numSvService,
	)

	env := &svMgrTestEnv{
		signVerifierManager: svm,
		inputTxBatch:        inputTxBatch,
		outputValidatedTxs:  outputValidatedTxs,
		mockVerifier:        verifier,
		grpcServers:         sc,
		policyManager:       pm,
	}

	return env
}

func (e *svMgrTestEnv) submitTxBatch(t *testing.T, numTxs int) dependencygraph.TxNodeBatch {
	t.Helper()
	blkNum := e.curBlockNum.Add(1) - 1
	txBatch, expectedValidatedTxs := createTxNodeBatchForTest(t, blkNum, numTxs, 0)
	channel.NewWriter(t.Context(), e.inputTxBatch).Write(txBatch)
	return expectedValidatedTxs
}

func (e *svMgrTestEnv) requireTxBatch(t *testing.T, expectedValidatedTxs dependencygraph.TxNodeBatch) {
	t.Helper()
	select {
	case actualTxBatch := <-e.outputValidatedTxs:
		require.ElementsMatch(t, expectedValidatedTxs, actualTxBatch)
	case <-time.After(5 * time.Second):
		t.Fatal("Did not receive block from output after timeout")
	}
}

func (e *svMgrTestEnv) requireConnectionMetrics(
	t *testing.T,
	svIndex, expectedConnStatus, expectedConnFailureTotal int,
) {
	t.Helper()
	require.Less(t, svIndex, len(e.signVerifierManager.signVerifier))
	sv := e.signVerifierManager.signVerifier[svIndex]
	monitoring.RequireConnectionMetrics(
		t, sv.conn.CanonicalTarget(),
		e.signVerifierManager.metrics.verifiersConnection,
		monitoring.ExpectedConn{Status: expectedConnStatus, FailureTotal: expectedConnFailureTotal},
	)
}

func (e *svMgrTestEnv) requireRetriedTxsTotal(t *testing.T, expectedRetriedTxsTotal int) {
	t.Helper()
	test.EventuallyIntMetric(
		t, expectedRetriedTxsTotal, e.signVerifierManager.metrics.verifiersRetriedTransactionTotal,
		30*time.Second, 250*time.Millisecond,
	)
}

func TestSignatureVerifierManagerWithSingleVerifier(t *testing.T) {
	t.Parallel()
	// Verifier marks valid and invalid flag as follows:
	// - when the block number is even, the even numbered txs are valid and the odd numbered txs are invalid
	// - when the block number is odd, the even numbered txs are invalid and the odd numbered txs are valid
	env := newSvMgrTestEnv(t, 1)

	expectedValidatedTxs := env.submitTxBatch(t, 10)
	env.requireTxBatch(t, expectedValidatedTxs)

	expectedValidatedTxs = env.submitTxBatch(t, 1)
	env.requireTxBatch(t, expectedValidatedTxs)

	expectedValidatedTxs = env.submitTxBatch(t, 4)
	env.requireTxBatch(t, expectedValidatedTxs)

	test.EventuallyIntMetric(
		t, 15, env.signVerifierManager.config.metrics.sigverifierTransactionProcessedTotal,
		30*time.Second, 10*time.Millisecond,
	)
}

func TestSignatureVerifierManagerWithLargeSize(t *testing.T) {
	t.Parallel()
	env := newSvMgrTestEnv(t, 1)

	expectedValidatedTxs := env.submitTxBatch(t, 1)
	env.requireTxBatch(t, expectedValidatedTxs)

	totalBlocks := 3
	txPerBlock := 50
	txBatches := make([]dependencygraph.TxNodeBatch, totalBlocks)
	expectedValidatedTxsBatches := make([]dependencygraph.TxNodeBatch, totalBlocks)
	for i := range 3 {
		//nolint:gosec // int -> uint64
		txBatches[i], expectedValidatedTxsBatches[i] = createTxNodeBatchForTest(t, uint64(i+1), txPerBlock, 1024*1024)
	}

	txsBatch := make(dependencygraph.TxNodeBatch, 0, totalBlocks*txPerBlock)
	for _, b := range txBatches {
		txsBatch = append(txsBatch, b...)
	}

	channel.NewWriter(t.Context(), env.inputTxBatch).Write(txsBatch)
	// env.requireTxBatch(t, expectedValidatedTxs)

	test.EventuallyIntMetric(
		t, totalBlocks*txPerBlock+1, env.signVerifierManager.config.metrics.sigverifierTransactionProcessedTotal,
		30*time.Second, 10*time.Millisecond,
	)
}

func TestSignatureVerifierManagerWithMultipleVerifiers(t *testing.T) {
	t.Parallel()
	env := newSvMgrTestEnv(t, 2)

	numTxs := 1
	numBlocks := 10_000
	expectedValidatedTxs := make([]dependencygraph.TxNodeBatch, numBlocks)
	for i := range numBlocks {
		expectedValidatedTxs[i] = env.submitTxBatch(t, numTxs)
	}

	deadline := time.After(time.Minute)
	// For each input batch in the input, we expect a batch in each of the two outputs.
	for range numBlocks {
		select {
		case txBatch := <-env.outputValidatedTxs:
			require.ElementsMatch(t, expectedValidatedTxs[txBatch[0].Tx.Ref.BlockNum], txBatch)
		case <-deadline:
			t.Fatal("Did not receive all blocks from output after timeout")
		}
	}

	verifierStreams := requireStreams(t, env.mockVerifier, 2)
	totalBlocksReceived := uint32(0)
	for _, sv := range verifierStreams {
		// Verify that each service got a reasonable proportion of the requests.
		totalBlocksReceived += sv.GetNumBlocksReceived()
		assert.Greater(t, sv.GetNumBlocksReceived(), uint32(0.1*float32(numBlocks)))
	}
	require.Equal(t, uint32(numBlocks), totalBlocksReceived)

	for _, sv := range env.signVerifierManager.signVerifier {
		sv.txMu.Lock()
		require.Empty(t, sv.txBeingValidated)
		sv.txMu.Unlock()
	}
}

func TestSignatureVerifierWithAllInvalidTxs(t *testing.T) {
	t.Parallel()
	txBatch := dependencygraph.TxNodeBatch{}
	expectedValidatedTxs := dependencygraph.TxNodeBatch{}
	for i := range 3 {
		txNode := &dependencygraph.TransactionNode{
			Tx: &servicepb.VcTx{
				Ref: committerpb.NewTxRef("", uint64(i), uint32(i)), //nolint:gosec
			},
		}
		txBatch = append(txBatch, txNode)

		expectedValidatedTxs = append(expectedValidatedTxs, &dependencygraph.TransactionNode{
			Tx: &servicepb.VcTx{
				Ref:                   txNode.Tx.Ref,
				PrelimInvalidTxStatus: &sigInvalidTxStatus,
			},
		})
	}

	env := newSvMgrTestEnv(t, 1)
	env.requireTxBatch(t, env.submitTxBatch(t, 1))

	channel.NewWriter(t.Context(), env.inputTxBatch).Write(txBatch)
	env.requireTxBatch(t, expectedValidatedTxs)
}

func createTxNodeBatchForTest(
	t *testing.T,
	blkNum uint64, numTxs, valueSize int,
) (inputTxBatch, expectedValidatedTxs dependencygraph.TxNodeBatch) {
	t.Helper()

	ns := []*applicationpb.TxNamespace{{
		BlindWrites: []*applicationpb.Write{{
			Value: utils.MustRead(rand.Reader, valueSize),
		}},
	}}
	for i := range numTxs {
		txNode := &dependencygraph.TransactionNode{
			Tx: &servicepb.VcTx{
				Ref:        committerpb.NewTxRef("", blkNum, uint32(i)), //nolint:gosec
				Namespaces: ns,
			},
		}

		switch i % 2 {
		case 0:
			// even number txs are valid.
			txNode.Endorsements = sigtest.CreateEndorsementsForThresholdRule([]byte("dummy"))
			expectedValidatedTxs = append(expectedValidatedTxs, txNode)
		case 1:
			// odd number txs are invalid. No signature means invalid transaction.
			// we need to create a copy of txNode to add expected status.
			txNodeWithStatus := &dependencygraph.TransactionNode{
				Tx: &servicepb.VcTx{
					Ref:                   txNode.Tx.Ref,
					Namespaces:            ns,
					PrelimInvalidTxStatus: &sigInvalidTxStatus,
				},
			}
			expectedValidatedTxs = append(expectedValidatedTxs, txNodeWithStatus)
		}

		inputTxBatch = append(inputTxBatch, txNode)
	}

	return inputTxBatch, expectedValidatedTxs
}

func TestSignatureVerifierManagerRecovery(t *testing.T) {
	t.Parallel()
	env := newSvMgrTestEnv(t, 1)
	verifierStreams := requireStreams(t, env.mockVerifier, 1)
	for _, sv := range verifierStreams {
		sv.MockFaultyNodeDropSize = 4
	}

	env.requireConnectionMetrics(t, 0, connection.Connected, 0)
	env.requireRetriedTxsTotal(t, 0)

	numTxs := 10
	expectedValidatedTxs := env.submitTxBatch(t, numTxs)

	// Validate the full block have not been reported
	firstSv := env.signVerifierManager.signVerifier[0]
	require.Eventually(t, func() bool {
		firstSv.txMu.Lock()
		defer firstSv.txMu.Unlock()
		return len(firstSv.txBeingValidated) == numTxs-6 // first 4 transactions would be pending
	}, 30*time.Second, 100*time.Millisecond)

	for i, s := range env.grpcServers.Servers {
		s.Stop()
		test.CheckServerStopped(t, env.grpcServers.Configs[i].Endpoint.Address())
	}
	env.requireConnectionMetrics(t, 0, connection.Disconnected, 1)

	for _, sv := range verifierStreams {
		sv.MockFaultyNodeDropSize = 0
	}
	env.grpcServers = mock.StartMockVerifierServiceFromServerConfig(
		t, env.mockVerifier, env.grpcServers.Configs...,
	)
	env.requireConnectionMetrics(t, 0, connection.Connected, 1)
	env.requireRetriedTxsTotal(t, 4)

	env.requireTxBatch(t, expectedValidatedTxs[4:])
	env.requireTxBatch(t, expectedValidatedTxs[:4])
}

func TestSignatureVerifierFatalDueToBadPolicy(t *testing.T) {
	t.Parallel()
	env := newSvMgrTestEnv(t, 1, []byte("failed to update policies")...)
	env.requireConnectionMetrics(t, 0, connection.Connected, 0)

	verifierStreams := requireStreams(t, env.mockVerifier, 1)
	sv := verifierStreams[0]

	// We require initial policy update.
	policyUpdateCount := sv.GetPolicyUpdateCounter()

	sv.SetReturnErrorForUpdatePolicies(true)
	env.policyManager.update(&servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{{Namespace: "$$$"}},
		},
	})

	require.Eventually(t, func() bool {
		// Process some batches to force progress.
		env.submitTxBatch(t, 1)
		return sv.GetPolicyUpdateCounter() > policyUpdateCount
	}, 2*time.Minute, 50*time.Millisecond)
}

func TestSignatureVerifierManagerPolicyUpdateAndRecover(t *testing.T) {
	t.Parallel()
	env := newSvMgrTestEnv(t, 3)
	verifierStreams := requireStreams(t, env.mockVerifier, 3)

	// verify that all mock policy verifiers have empty verification key.
	for i, mockSv := range verifierStreams {
		env.requireConnectionMetrics(t, i, connection.Connected, 0)
		require.Empty(t, mockSv.GetUpdates())
	}

	// set verification key
	ns1Policy, _ := policy.MakePolicyAndNsEndorser(t, "ns1")
	ns2Policy, _ := policy.MakePolicyAndNsEndorser(t, "ns2")
	expectedUpdate := &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{ns1Policy, ns2Policy},
		},
		Config: &applicationpb.ConfigTransaction{
			Envelope: []byte("config1"),
		},
	}
	env.policyManager.update(expectedUpdate)

	t.Log("Verify that all mock policy verifiers have the same verification key")
	env.requireAllUpdate(t, verifierStreams, 1, expectedUpdate)

	t.Log("Stop the service")
	env.grpcServers.Servers[0].Stop()
	test.CheckServerStopped(t, env.grpcServers.Configs[0].Endpoint.Address())
	env.submitTxBatch(t, 1)
	env.requireConnectionMetrics(t, 0, connection.Disconnected, 1)

	t.Log("Update policy manager")
	ns2NewPolicy, _ := policy.MakePolicyAndNsEndorser(t, "ns2")
	expectedSecondUpdate := &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{ns2NewPolicy},
		},
		Config: &applicationpb.ConfigTransaction{
			Envelope: []byte("config2"),
		},
	}

	env.policyManager.update(expectedSecondUpdate)

	verifierStreams = requireStreams(t, env.mockVerifier, 2)
	// Ensure the correct server is shutdown.
	for _, mockSv := range verifierStreams {
		require.NotEqual(t, env.grpcServers.Configs[0].Endpoint.Address(), mockSv.ServerEndpoint())
	}
	t.Log("Verify that all other mock policy verifiers have the same verification key")
	env.requireAllUpdate(t, verifierStreams, 2, expectedSecondUpdate)

	t.Log("Ensure the down SV is not updated")
	require.Never(t, func() bool {
		// Process some batches to force progress
		env.submitTxBatch(t, 1)
		return len(verifierStreams[0].GetUpdates()) > 2
	}, 2*time.Second, 1*time.Second)

	t.Log("Restart server")
	env.grpcServers.Servers[0] = mock.StartMockVerifierServiceFromServerConfig(
		t, env.mockVerifier, env.grpcServers.Configs[0],
	).Servers[0]
	env.requireConnectionMetrics(t, 0, connection.Connected, 1)
	t.Log("New instance is up")

	newExpectedUpdate := &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{ns1Policy, ns2NewPolicy},
		},
		Config: &applicationpb.ConfigTransaction{
			Envelope: []byte("config2"),
		},
	}

	verifierStreams = requireStreams(t, env.mockVerifier, 3)
	// We only check the newest stream (they are ordered by the time they are created).
	require.Equal(t, env.grpcServers.Configs[0].Endpoint.Address(), verifierStreams[2].ServerEndpoint())
	env.requireAllUpdate(t, verifierStreams[2:], 1, newExpectedUpdate)
}

func (e *svMgrTestEnv) requireAllUpdate(
	t *testing.T,
	svs []*mock.VerifierStreamState,
	expectedCount int,
	expected *servicepb.VerifierUpdates,
) {
	t.Helper()
	updates := make([][]*servicepb.VerifierUpdates, len(svs))

	// verify that all mock policy verifiers have the same verification key.
	require.Eventually(t, func() bool {
		defer func() {
			// Process some batches to force progress.
			for range svs {
				e.submitTxBatch(t, 1)
			}
		}()

		// We iterate all of them everytime to ensure we hold the quick workers.
		isDone := true
		for i, sv := range svs {
			if updates[i] != nil {
				continue
			}

			u := sv.GetUpdates()
			if len(u) < expectedCount {
				isDone = false
				continue
			}
			updates[i] = u
		}
		return isDone
	}, 2*time.Minute, 10*time.Millisecond)

	for _, u := range updates {
		require.Len(t, u, expectedCount)
		requireUpdateEqual(t, expected, u[expectedCount-1])
	}
}

func requireStreams(t *testing.T, mockVerifier *mock.Verifier, expectedNumStreams int) []*mock.VerifierStreamState {
	t.Helper()
	var verifierStreams []*mock.VerifierStreamState
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		verifierStreams = mockVerifier.Streams()
		require.Len(ct, verifierStreams, expectedNumStreams)
	}, 15*time.Second, 10*time.Millisecond)
	return verifierStreams
}
