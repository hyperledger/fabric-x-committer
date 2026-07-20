/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"crypto/rand"
	"sync"
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
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

type svMgrTestEnv struct {
	signVerifierManager *signatureVerifierManager
	inputTxBatch        chan dependencygraph.TxNodeBatch
	outputValidatedTxs  chan dependencygraph.TxNodeBatch
	mockVerifier        *mock.Verifier
	grpcServers         *test.Servers
	policyManager       *policyManager
	curBlockNum         atomic.Uint64
}

func newSvMgrTestEnv(t *testing.T, numSvService int, expectedEndErrorMsg ...byte) *svMgrTestEnv {
	t.Helper()
	expectedEndError := string(expectedEndErrorMsg)
	verifier, sc := mock.StartMockVerifierService(t, test.StartServerParameters{NumService: numSvService})

	inputTxBatch := make(chan dependencygraph.TxNodeBatch, 10)
	outputValidatedTxs := make(chan dependencygraph.TxNodeBatch, 10)

	pm := newPolicyManager()
	svm := newSignatureVerifierManager(
		&signVerifierManagerConfig{
			clientConfig:             test.ServerToMultiClientConfig(test.InsecureTLSConfig, sc.Configs...),
			incomingTxsForValidation: inputTxBatch,
			outgoingValidatedTxs:     outputValidatedTxs,
			metrics:                  newPerformanceMetrics(),
			policyManager:            pm,
		},
	)

	test.RunServiceForTest(
		t.Context(), t,
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
		t, svm.metrics.Provider, "coordinator_verifier_connection_status", numSvService,
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
	return e.submitTxBatchWithContext(t.Context(), t, numTxs)
}

func (e *svMgrTestEnv) submitTxBatchWithContext(
	ctx context.Context, t *testing.T, numTxs int,
) dependencygraph.TxNodeBatch {
	t.Helper()
	blkNum := e.curBlockNum.Add(1) - 1
	txBatch, expectedValidatedTxs := createTxNodeBatchForTest(t, blkNum, numTxs, 0)
	channel.NewWriter(ctx, e.inputTxBatch).Write(txBatch)
	return expectedValidatedTxs
}

// startBatchSubmitter runs a background goroutine that submits batchesPerRound batches every
// batchSubmitInterval until the returned cancelled function is called.
// Sending batches is what pushes a pending policy update out to the verifiers,
// so a test that waits for propagation starts the submitter, runs its require.Eventually /
// require.Never, then stops it.
//
// The batch submission lives here, in a dedicated and explicitly-stopped goroutine, and must NOT
// be moved into the require.Eventually/require.Never condition. testify runs the condition in its
// own goroutine ("go checkCond()") and, once waitFor elapses, returns WITHOUT waiting for that
// goroutine to finish (see testify assertions.go: the <-timer.C case returns immediately and the
// buffered result channel lets the orphan complete later). submitTxBatch blocks while the input
// channel is full, so a condition that submits leaks a goroutine past the assertion that then
// (1) submits stray batches into a later phase of the test and (2) races on any variable the
// condition captured -- an observed data race on verifierStreams.
// Keeping the condition side-effect-free (a pure read of atomics/stable locals) makes it safe
// even if it outlives the assertion.
func (e *svMgrTestEnv) startBatchSubmitter(t *testing.T, batchesPerRound int) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for range batchesPerRound {
					e.submitTxBatchWithContext(ctx, t, 1)
				}
			}
		}
	})
	t.Cleanup(wg.Wait)
	t.Cleanup(cancel)
	return cancel
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
	numBlocks := uint64(10_000)
	expectedValidatedTxs := make([]dependencygraph.TxNodeBatch, numBlocks)
	for i := range numBlocks {
		expectedValidatedTxs[i] = env.submitTxBatch(t, numTxs)
	}

	deadline := time.After(time.Minute)
	// For each input batch in the input, we expect a batch in each of the two outputs.
	for range numBlocks {
		select {
		case txBatch := <-env.outputValidatedTxs:
			require.ElementsMatch(t, expectedValidatedTxs[txBatch[0].VCTx.Ref.BlockNum], txBatch)
		case <-deadline:
			t.Fatal("Did not receive all blocks from output after timeout")
		}
	}

	verifierStreams := mock.RequireStreams(t, env.mockVerifier, 2)
	totalBlocksReceived := uint64(0)
	for _, sv := range verifierStreams {
		// Verify that each service got a reasonable proportion of the requests.
		totalBlocksReceived += sv.NumBlocksReceived.Load()
		assert.Greater(t, sv.NumBlocksReceived.Load(), uint64(0.1*float32(numBlocks)))
	}
	require.Equal(t, numBlocks, totalBlocksReceived)

	for _, sv := range env.signVerifierManager.signVerifier {
		sv.txMu.Lock()
		require.Empty(t, sv.txBeingValidated)
		sv.txMu.Unlock()
	}
}

func TestSignatureVerifierWithAllInvalidTxs(t *testing.T) {
	t.Parallel()
	txBatch := make([]*dependencygraph.TransactionNode, 0, 3)
	expectedValidatedTxs := make([]*dependencygraph.TransactionNode, 0, 3)
	for i := range 3 {
		ref := committerpb.NewTxRef("", uint64(i), uint32(i))
		txNode := &dependencygraph.TransactionNode{
			VCTx: &servicepb.VcTx{Ref: ref},
			VerifierTx: &servicepb.TxWithRef{
				Ref:     ref,
				Content: &applicationpb.Tx{},
			},
		}
		txBatch = append(txBatch, txNode)

		expectedValidatedTxs = append(expectedValidatedTxs, &dependencygraph.TransactionNode{
			VCTx: &servicepb.VcTx{
				Ref:                   txNode.VCTx.Ref,
				PrelimInvalidTxStatus: &sigInvalidTxStatus,
			},
			VerifierTx: txNode.VerifierTx,
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
		txWithRef := &servicepb.TxWithRef{
			Ref: committerpb.NewTxRef("", blkNum, uint32(i)),
			Content: &applicationpb.Tx{
				Namespaces: ns,
			},
		}
		txNode := &dependencygraph.TransactionNode{
			VerifierTx: txWithRef,
			VCTx: &servicepb.VcTx{
				Ref:        txWithRef.Ref,
				Namespaces: ns,
			},
		}

		switch i % 2 {
		case 0:
			// even number txs are valid.
			txNode.VerifierTx.Content.Endorsements = testsig.CreateEndorsementsForThresholdRule([]byte("dummy"))
			expectedValidatedTxs = append(expectedValidatedTxs, txNode)
		case 1:
			// odd number txs are invalid. No signature means invalid transaction.
			// we need to create a copy of txNode to add expected status.
			txNodeWithStatus := &dependencygraph.TransactionNode{
				VCTx: &servicepb.VcTx{
					Ref:                   txWithRef.Ref,
					Namespaces:            ns,
					PrelimInvalidTxStatus: &sigInvalidTxStatus,
				},
				VerifierTx: txWithRef,
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
	verifierStreams := mock.RequireStreams(t, env.mockVerifier, 1)
	for _, sv := range verifierStreams {
		sv.MockFaultyNodeDropSize.Store(4)
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

	for _, stopFunc := range env.grpcServers.ServersStop {
		stopFunc()
	}
	for _, c := range env.grpcServers.Configs {
		test.CheckServerStopped(t, c.GRPC.Endpoint.Address())
	}
	env.requireConnectionMetrics(t, 0, connection.Disconnected, 1)

	for _, sv := range verifierStreams {
		sv.MockFaultyNodeDropSize.Store(0)
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

	verifierStreams := mock.RequireStreams(t, env.mockVerifier, 1)
	sv := verifierStreams[0]

	// We require initial policy update.
	policyUpdateCount := sv.PolicyUpdateCounter.Load()

	sv.ReturnErrForUpdatePolicies.Store(true)
	env.policyManager.update(&servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{{Namespace: "$$$"}},
		},
	})

	require.Eventually(t, func() bool {
		// Process some batches to force progress.
		env.submitTxBatch(t, 1)
		return sv.PolicyUpdateCounter.Load() > policyUpdateCount
	}, 2*time.Minute, 50*time.Millisecond)
}

func TestSignatureVerifierManagerPolicyUpdateAndRecover(t *testing.T) {
	t.Parallel()
	env := newSvMgrTestEnv(t, 3)
	verifierStreams := mock.RequireStreams(t, env.mockVerifier, 3)

	// verify that all mock policy verifiers have empty verification key.
	for i, mockSv := range verifierStreams {
		env.requireConnectionMetrics(t, i, connection.Connected, 0)
		require.Empty(t, *mockSv.Updates.Load())
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

	// Drain the output channel in the background to prevent signVerifier goroutines from blocking
	// on channel writes. Without this, goroutines stuck on Write() never reach Recv(), so they
	// can't detect server failures via broken streams. Starting the drain before requireAllUpdate
	// ensures signVerifiers never block on Write during batch processing, keeping them responsive
	// to stream errors when the server is stopped later.
	drainReady := channel.NewReady()
	go func() {
		drainReady.SignalReady()
		for {
			select {
			case <-t.Context().Done():
				return
			case <-env.outputValidatedTxs:
			}
		}
	}()
	require.True(t, drainReady.WaitForReady(t.Context()))

	t.Log("Verify that all mock policy verifiers have the same verification key")
	env.requireAllUpdate(t, verifierStreams, 1, expectedUpdate)

	t.Log("Stop the service")
	env.grpcServers.ServersStop[0]()
	test.CheckServerStopped(t, env.grpcServers.Configs[0].GRPC.Endpoint.Address())
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

	t.Log("Ensure the correct server is shutdown")
	verifierStreams = mock.RequireStreams(t, env.mockVerifier, 2)
	stoppedEndpoint := env.grpcServers.Configs[0].GRPC.Endpoint.Address()
	mock.RequireStreamsWithEndpoints(t, env.mockVerifier, 0, stoppedEndpoint)

	t.Log("Verify that all other mock policy verifiers have the same verification key")
	env.requireAllUpdate(t, verifierStreams, 2, expectedSecondUpdate)

	t.Log("Ensure the surviving verifiers receive no further updates")
	survivingStream := verifierStreams[0]
	stopSubmitter := env.startBatchSubmitter(t, 1)
	require.Never(t, func() bool {
		return len(*survivingStream.Updates.Load()) > 2
	}, 2*time.Second, 100*time.Millisecond)
	stopSubmitter()

	t.Log("Restart server")
	env.grpcServers.ServersStop[0] = mock.StartMockVerifierServiceFromServerConfig(
		t, env.mockVerifier, env.grpcServers.Configs[0],
	).ServersStop[0]
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

	t.Log("Ensure we re-connected to the endpoint")
	// Barrier: wait until all three streams are active again. The result is unused.
	mock.RequireStreams(t, env.mockVerifier, 3)
	restartedStream := mock.RequireStreamsWithEndpoints(t, env.mockVerifier, 1, stoppedEndpoint)
	env.requireAllUpdate(t, restartedStream, 1, newExpectedUpdate)
}

func (e *svMgrTestEnv) requireAllUpdate(
	t *testing.T,
	svs []*mock.VerifierStreamState,
	expectedCount int,
	expected *servicepb.VerifierUpdates,
) {
	t.Helper()

	// Sending batches is what pushes the pending policy update to the verifiers. Submit in the
	// background so the require.Eventually condition below stays a pure, non-blocking read (see
	// startBatchSubmitter for why the submission must not live inside the condition).
	stop := e.startBatchSubmitter(t, len(svs))
	require.Eventually(t, func() bool {
		for _, sv := range svs {
			if len(*sv.Updates.Load()) < expectedCount {
				return false
			}
		}
		return true
	}, 2*time.Minute, 10*time.Millisecond)
	stop()

	// No further updates propagate once the submitter is stopped (no new policy is pushed here),
	// so re-reading each verifier's updates now is stable.
	for _, sv := range svs {
		u := *sv.Updates.Load()
		require.Len(t, u, expectedCount)
		requireUpdateEqual(t, expected, u[expectedCount-1])
	}
}
