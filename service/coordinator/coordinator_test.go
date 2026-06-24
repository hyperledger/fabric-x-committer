/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"maps"
	"math"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testapp"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

type (
	coordinatorTestEnv struct {
		coordinator            *Service
		config                 *Config
		serverConfig           *serve.Config
		client                 servicepb.CoordinatorClient
		csStream               servicepb.Coordinator_BlockProcessingClient
		streamCancel           context.CancelFunc
		verifier               *mock.Verifier
		vc                     *mock.VcService
		sigVerifierGrpcServers *test.Servers
		serverTLS              connection.TLSConfig
		clientTLS              connection.TLSConfig
	}

	testConfig struct {
		numSigService int
		numVcService  int

		serverTLS connection.TLSConfig
		clientTLS connection.TLSConfig
	}
)

// TestCoordinatorSecureConnection verifies the Coordinator gRPC server's behavior
// under various client TLS configurations.
func TestCoordinatorSecureConnection(t *testing.T) {
	t.Parallel()
	test.RunSecureConnectionTest(
		t,
		func(t *testing.T, serverTLSCfg, clientTLSCfg connection.TLSConfig) test.RPCAttempt {
			t.Helper()
			env := newCoordinatorTestEnv(t, &testConfig{
				numSigService: 1,
				numVcService:  1,
				serverTLS:     serverTLSCfg,
				clientTLS:     clientTLSCfg,
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
			t.Cleanup(cancel)
			env.startService(ctx, t)
			return func(ctx context.Context, t *testing.T, cfg connection.TLSConfig) error {
				t.Helper()
				client := createCoordinatorClientWithTLS(t, &env.serverConfig.GRPC.Endpoint, cfg)
				_, err := client.GetNextBlockNumberToCommit(ctx, nil)
				return err
			}
		},
	)
}

func newCoordinatorTestEnv(t *testing.T, tConfig *testConfig) *coordinatorTestEnv {
	t.Helper()

	verifier, svServers := mock.StartMockVerifierService(t, test.StartServerParameters{
		NumService: tConfig.numSigService,
		TLSConfig:  tConfig.serverTLS,
	})
	vcService, vcServers := mock.StartMockVCService(t, test.StartServerParameters{
		NumService: tConfig.numVcService,
		TLSConfig:  tConfig.serverTLS,
	})

	c := &Config{
		Verifier:           *test.ServerToMultiClientConfig(tConfig.clientTLS, svServers.Configs...),
		ValidatorCommitter: *test.ServerToMultiClientConfig(tConfig.clientTLS, vcServers.Configs...),
		DependencyGraph: &DependencyGraphConfig{
			NumOfLocalDepConstructors: 3,
			WaitingTxsLimit:           10,
		},
		ChannelBufferSizePerGoroutine: 2000,
	}
	return &coordinatorTestEnv{
		coordinator:            NewCoordinatorService(c),
		config:                 c,
		verifier:               verifier,
		vc:                     vcService,
		sigVerifierGrpcServers: svServers,
		serverTLS:              tConfig.serverTLS,
		clientTLS:              tConfig.clientTLS,
	}
}

func (e *coordinatorTestEnv) startServiceAndOpenStream(ctx context.Context, t *testing.T) {
	t.Helper()
	e.startService(ctx, t)
	e.client = createCoordinatorClientWithTLS(t, &e.serverConfig.GRPC.Endpoint, e.clientTLS)

	sCtx, sCancel := context.WithTimeout(ctx, 5*time.Minute)
	t.Cleanup(sCancel)
	csStream, err := e.client.BlockProcessing(sCtx)
	require.NoError(t, err)

	e.csStream = csStream
	e.streamCancel = sCancel
}

func (e *coordinatorTestEnv) startService(
	ctx context.Context,
	t *testing.T,
) {
	t.Helper()
	cs := e.coordinator
	e.serverConfig = test.NewLocalHostServiceConfig(e.serverTLS)
	test.RunServiceAndServeForTest(ctx, t, cs, e.serverConfig)
}

func (e *coordinatorTestEnv) ensureStreamActive(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		if !e.coordinator.streamActive.TryLock() {
			return true
		}
		defer e.coordinator.streamActive.Unlock()
		return false
	}, 4*time.Second, 250*time.Millisecond)
}

func (e *coordinatorTestEnv) createNamespaces(t *testing.T, blkNum int, nsIDs ...string) {
	t.Helper()
	pBytes, err := proto.Marshal(policy.MakeECDSAThresholdRuleNsPolicy([]byte("publicKey")))
	require.NoError(t, err)

	blockNum := uint64(blkNum) //nolint:gosec // int -> uint64.
	b0 := &servicepb.CoordinatorBatch{
		Txs: []*servicepb.TxWithRef{
			{
				Ref: committerpb.NewTxRef(uuid.NewString(), blockNum, 0),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId: committerpb.ConfigNamespaceID,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   []byte(committerpb.ConfigKey),
							Value: pBytes,
						}},
					}},
				},
			},
		},
	}

	b1 := &servicepb.CoordinatorBatch{}
	for i, nsID := range nsIDs {
		b1.Txs = append(b1.Txs, &servicepb.TxWithRef{
			Ref: committerpb.NewTxRef(uuid.NewString(), blockNum, uint32(i+1)), //nolint:gosec // int -> uint32.
			Content: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:      committerpb.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*applicationpb.ReadWrite{{
						Key:   []byte(nsID),
						Value: pBytes,
					}},
				}},
			},
		})
	}

	for _, b := range []*servicepb.CoordinatorBatch{b0, b1} {
		for _, tx := range b.Txs {
			// The mock verifier verifies that len(tx.Namespace)==len(tx.Signatures)
			tx.Content.Endorsements = make([]*applicationpb.Endorsements, len(tx.Content.Namespaces))
		}

		err = e.csStream.Send(b)
		require.NoError(t, err)
		status := make([]*committerpb.TxStatus, 0, len(b.Txs))
		require.Eventually(t, func() bool {
			txStatus, receiveErr := e.csStream.Recv()
			require.NoError(t, receiveErr)
			require.NotNil(t, txStatus)
			require.NotNil(t, txStatus.Status)
			status = append(status, txStatus.Status...)
			return len(status) == len(b.Txs)
		}, 2*time.Minute, 10*time.Millisecond)

		for _, s := range status {
			require.Equal(t, committerpb.Status_COMMITTED.String(), s.Status.String())
		}
	}
}

func TestCoordinatorOneActiveStreamOnly(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)

	env.ensureStreamActive(t)

	stream, err := env.client.BlockProcessing(ctx)
	require.NoError(t, err)
	_, err = stream.Recv()
	require.ErrorContains(t, err, ErrExistingStreamOrConflictingOp.Error())
}

func TestGetNextBlockNumWithActiveStream(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)

	env.ensureStreamActive(t)

	_, err := env.client.GetNextBlockNumberToCommit(ctx, nil)
	require.NoError(t, err)
}

func TestCoordinatorServiceValidTx(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 2, numVcService: 2})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)

	env.createNamespaces(t, 0, "1")

	preMetricsValue := test.GetIntMetricValue(t, env.coordinator.metrics.transactionReceivedTotal)

	pBytes, err := proto.Marshal(policy.MakeECDSAThresholdRuleNsPolicy([]byte("publicKey")))
	require.NoError(t, err)
	err = env.csStream.Send(&servicepb.CoordinatorBatch{
		Txs: []*servicepb.TxWithRef{
			{
				Ref: committerpb.NewTxRef("tx1", 1, 0),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*applicationpb.ReadWrite{
								{
									Key: []byte("key"),
								},
							},
						},
						{
							NsId:      committerpb.MetaNamespaceID,
							NsVersion: 0,
							ReadWrites: []*applicationpb.ReadWrite{
								{
									Key:   []byte("2"),
									Value: pBytes,
								},
							},
						},
					},
					Endorsements: make([]*applicationpb.Endorsements, 2),
				},
			},
		},
	})
	require.NoError(t, err)
	test.EventuallyIntMetric(
		t, preMetricsValue+1, env.coordinator.metrics.transactionReceivedTotal,
		1*time.Second, 100*time.Millisecond,
	)

	env.requireStatus(ctx, t, []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx1", 1, 0),
	}, nil)

	test.RequireIntMetricValue(t, preMetricsValue+1, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		committerpb.Status_COMMITTED.String(),
	))

	_, err = env.coordinator.SetLastCommittedBlockNumber(ctx, &servicepb.BlockRef{Number: 1})
	require.NoError(t, err)

	nextBlock, err := env.coordinator.GetNextBlockNumberToCommit(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, nextBlock)
	require.Equal(t, uint64(2), nextBlock.Number)
}

func TestNoPendingTransactionProcessing(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1})

	// Seven transactions are still waiting for sidecar-visible final status. Five of
	// those statuses are already queued in two batches. One queued status is a
	// rejected transaction, proving rejected statuses are counted in the same unit
	// as committed statuses.
	env.coordinator.numTxsInProgress.Store(7)

	require.True(t, env.coordinator.queues.vcServiceToCoordinatorTxStatus.write(t.Context(), &committerpb.TxStatusBatch{
		Status: []*committerpb.TxStatus{
			committerpb.NewTxStatus(committerpb.Status_COMMITTED, "queued-1", 2, 0),
			committerpb.NewTxStatus(committerpb.Status_COMMITTED, "queued-2", 2, 1),
		},
	}))
	require.True(t, env.coordinator.queues.vcServiceToCoordinatorTxStatus.write(t.Context(), &committerpb.TxStatusBatch{
		Status: []*committerpb.TxStatus{
			committerpb.NewTxStatus(committerpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD, "queued-rejected", 2, 2),
			committerpb.NewTxStatus(committerpb.Status_COMMITTED, "queued-4", 2, 3),
			committerpb.NewTxStatus(committerpb.Status_COMMITTED, "queued-5", 2, 4),
		},
	}))

	idle, err := env.coordinator.NoPendingTransactionProcessing(t.Context(), nil)
	require.NoError(t, err)
	require.False(t, idle.GetValue())
}

func TestCoordinatorServiceDependentOrderedTxs(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 2, numVcService: 2})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)

	utNsID := "1"
	utNsVersion := uint64(0)
	mainKey := []byte("main-key")
	subKey := []byte("sub-key")
	pBytes, err := proto.Marshal(policy.MakeECDSAThresholdRuleNsPolicy([]byte("publicKey")))
	require.NoError(t, err)

	b0 := &servicepb.CoordinatorBatch{
		Txs: []*servicepb.TxWithRef{
			{
				Ref: committerpb.NewTxRef("config TX", 0, 0),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId: committerpb.ConfigNamespaceID,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   []byte(committerpb.ConfigKey),
							Value: []byte("config"),
						}},
					}},
				},
			},
		},
	}

	// We send a block with a series of TXs with apparent conflicts, but all should be committed successfully if
	// executed serially.
	b1 := &servicepb.CoordinatorBatch{
		Txs: []*servicepb.TxWithRef{
			{
				Ref: committerpb.NewTxRef("config TX", 0, 0),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId: committerpb.ConfigNamespaceID,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   []byte(committerpb.ConfigKey),
							Value: []byte("config"),
						}},
					}},
				},
			},
			{
				Ref: committerpb.NewTxRef("create namespace 1", 0, 1),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      committerpb.MetaNamespaceID,
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   []byte(utNsID),
							Value: pBytes,
						}},
					}},
				},
			},
			{
				Ref: committerpb.NewTxRef("create main key (read-write version 0)", 0, 2),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      utNsID,
						NsVersion: utNsVersion,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   mainKey,
							Value: []byte("value of version 0"),
						}},
					}},
				},
			},
			{
				Ref: committerpb.NewTxRef("update main key (read-write version 1)", 0, 3),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      utNsID,
						NsVersion: utNsVersion,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:     mainKey,
							Value:   []byte("value of version 1"),
							Version: new(uint64(0)),
						}},
					}},
				},
			},
			{
				Ref: committerpb.NewTxRef("update main key (blind-write version 2)", 0, 4),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      utNsID,
						NsVersion: utNsVersion,
						BlindWrites: []*applicationpb.Write{{
							Key:   mainKey,
							Value: []byte("Value of version 2"),
						}},
					}},
				},
			},
			{
				Ref: committerpb.NewTxRef("read main key, create sub key (read version 2, read-write version 0)", 0, 5),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      utNsID,
						NsVersion: utNsVersion,
						ReadsOnly: []*applicationpb.Read{{
							Key:     mainKey,
							Version: new(uint64(2)),
						}},
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   subKey,
							Value: []byte("Sub value of version 0"),
						}},
					}},
				},
			},
			{
				Ref: committerpb.NewTxRef("update main key (read-write version 3)", 0, 6),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      utNsID,
						NsVersion: utNsVersion,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:     mainKey,
							Version: new(uint64(2)),
							Value:   []byte("Value of version 3"),
						}},
					}},
				},
			},
		},
	}

	for _, b := range []*servicepb.CoordinatorBatch{b0, b1} {
		for _, tx := range b.Txs {
			tx.Content.Endorsements = testsig.CreateEndorsementsForThresholdRule([]byte("dummy"))
		}

		expectedReceived := test.GetIntMetricValue(t, env.coordinator.metrics.transactionReceivedTotal) + len(b.Txs)

		require.NoError(t, env.csStream.Send(b))
		require.Eventually(t, func() bool {
			return test.GetIntMetricValue(t, env.coordinator.metrics.transactionReceivedTotal) >= expectedReceived
		}, time.Minute, 500*time.Millisecond)

		status := env.receiveStatus(t, len(b.Txs))
		for txID, txStatus := range status {
			require.Equal(t, committerpb.Status_COMMITTED, txStatus.Status, txID)
		}
		test.RequireIntMetricValue(t, expectedReceived,
			env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(committerpb.Status_COMMITTED.String()))
	}

	// Assert that the dependent TX chain reached the mock VC in the correct
	// relative order. The dependency graph releases a TX to the VC only after
	// its predecessor commits, so arrival order at the VC reflects execution order.
	// idx 5 and idx 6 both depend only on idx 4 (both read main-key v2); their
	// order relative to each other is not guaranteed, so we only assert both come
	// after idx 4.
	order := env.vc.GetReceivedTxOrder()
	pos := func(txID string) int {
		p := slices.Index(order, txID)
		require.GreaterOrEqualf(t, p, 0, "dependent TX %q never received by VC; order: %v", txID, order)
		return p
	}

	ns := pos("create namespace 1")
	create := pos("create main key (read-write version 0)")
	updateV1 := pos("update main key (read-write version 1)")
	blindV2 := pos("update main key (blind-write version 2)")
	readV2 := pos("read main key, create sub key (read version 2, read-write version 0)")
	updateV3 := pos("update main key (read-write version 3)")

	require.Less(t, ns, create, "namespace must precede main-key TXs; order: %v", order)
	require.Less(t, create, updateV1, "order: %v", order)
	require.Less(t, updateV1, blindV2, "order: %v", order)
	require.Less(t, blindV2, readV2, "v2 reader must follow blind-write; order: %v", order)
	require.Less(t, blindV2, updateV3, "v2 writer must follow blind-write; order: %v", order)
}

func TestQueueSize(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 2, numVcService: 2})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	go env.coordinator.monitorQueues(ctx)

	q := env.coordinator.queues
	m := env.coordinator.metrics
	q.depGraphToSigVerifierFreeTxs <- dependencygraph.TxNodeBatch{}
	q.sigVerifierToVCServiceValidatedTxs <- dependencygraph.TxNodeBatch{}
	q.vcServiceToDepGraphValidatedTxs <- dependencygraph.TxNodeBatch{}
	require.True(t, q.vcServiceToCoordinatorTxStatus.write(t.Context(), &committerpb.TxStatusBatch{}))

	require.Eventually(t, func() bool {
		return test.GetIntMetricValue(t, m.sigverifierInputTxBatchQueueSize) == 1 &&
			test.GetIntMetricValue(t, m.sigverifierOutputValidatedTxBatchQueueSize) == 1 &&
			test.GetIntMetricValue(t, m.vcserviceOutputValidatedTxBatchQueueSize) == 1 &&
			test.GetIntMetricValue(t, m.vcserviceOutputTxStatusBatchQueueSize) == 1
	}, 3*time.Second, 500*time.Millisecond)

	<-q.depGraphToSigVerifierFreeTxs
	<-q.sigVerifierToVCServiceValidatedTxs
	<-q.vcServiceToDepGraphValidatedTxs
	_, ok := q.vcServiceToCoordinatorTxStatus.read(t.Context())
	require.True(t, ok)

	require.Eventually(t, func() bool {
		return test.GetIntMetricValue(t, m.sigverifierInputTxBatchQueueSize) == 0 &&
			test.GetIntMetricValue(t, m.sigverifierOutputValidatedTxBatchQueueSize) == 0 &&
			test.GetIntMetricValue(t, m.vcserviceOutputValidatedTxBatchQueueSize) == 0 &&
			test.GetIntMetricValue(t, m.vcserviceOutputTxStatusBatchQueueSize) == 0
	}, 3*time.Second, 500*time.Millisecond)
}

func TestCoordinatorRecovery(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)

	// The mock VC does not validate; the test injects the non-committed outcomes it expects.
	// "mvcc conflict" (2,2) conflicts, and "tx1" (2,5) reuses the TX ID committed in block 1.
	env.vc.SetTxStatus(committerpb.NewTxRef("mvcc conflict", 2, 2), committerpb.Status_ABORTED_MVCC_CONFLICT)
	env.vc.SetTxStatus(committerpb.NewTxRef("tx1", 2, 5), committerpb.Status_REJECTED_DUPLICATE_TX_ID)

	env.createNamespaces(t, 0, "1")

	err := env.csStream.Send(&servicepb.CoordinatorBatch{
		Txs: []*servicepb.TxWithRef{{
			Ref: committerpb.NewTxRef("tx1", 1, 0),
			Content: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*applicationpb.ReadWrite{{
						Key:   []byte("key1"),
						Value: []byte("value1"),
					}},
				}},
				Endorsements: make([]*applicationpb.Endorsements, 1),
			},
		}},
	})
	require.NoError(t, err)

	env.requireStatus(ctx, t, []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx1", 1, 0),
	}, nil)

	_, err = env.client.SetLastCommittedBlockNumber(ctx, &servicepb.BlockRef{Number: 1})
	require.NoError(t, err)

	nextBlock, err := env.client.GetNextBlockNumberToCommit(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, nextBlock)
	require.Equal(t, uint64(2), nextBlock.Number)

	// To simulate a failure scenario in which a block is partially committed, we first create block 2
	// with two transaction but actual block 2 is supposed to have four transactions. Once the partial block 2
	// is committed, we will restart the service and send a full block 2 with all four transactions.
	nsPolicy, err := proto.Marshal(policy.MakeECDSAThresholdRuleNsPolicy([]byte("publicKey")))
	require.NoError(t, err)
	block2 := &servicepb.CoordinatorBatch{
		Txs: []*servicepb.TxWithRef{
			{
				Ref: committerpb.NewTxRef("tx2", 2, 0),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "1",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key: []byte("key2"),
						}},
					}},
					Endorsements: make([]*applicationpb.Endorsements, 1),
				},
			},
			{
				Ref: committerpb.NewTxRef("mvcc conflict", 2, 2),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "2",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key: []byte("key3"),
						}},
					}},
					Endorsements: make([]*applicationpb.Endorsements, 1),
				},
			},
			{
				Ref: committerpb.NewTxRef("tx1", 2, 5),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "1",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   []byte("key1"),
							Value: []byte("value1"),
						}},
					}},
					Endorsements: make([]*applicationpb.Endorsements, 1),
				},
			},
		},
	}
	require.NoError(t, env.csStream.Send(block2))

	expectedTxStatus := []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx2", 2, 0),
		committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "mvcc conflict", 2, 2),
		committerpb.NewTxStatus(committerpb.Status_REJECTED_DUPLICATE_TX_ID, "tx1", 2, 5),
	}
	env.requireStatus(ctx, t, expectedTxStatus, []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx1", 1, 0),
	})

	cancel()

	env.coordinator = NewCoordinatorService(env.config)
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)

	statusExistsForNonDuplicateTxID(t, env.vc, expectedTxStatus)

	// Now, we are sending the full block 2.
	block2 = &servicepb.CoordinatorBatch{
		Txs: []*servicepb.TxWithRef{
			{
				Ref: committerpb.NewTxRef("tx2", 2, 0),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "1",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key: []byte("key2"),
						}},
					}},
					Endorsements: make([]*applicationpb.Endorsements, 1),
				},
			},
			{
				Ref: committerpb.NewTxRef("tx3", 2, 1),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "1",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key: []byte("key3"),
						}},
					}},
					Endorsements: make([]*applicationpb.Endorsements, 1),
				},
			},
			{
				Ref: committerpb.NewTxRef("mvcc conflict", 2, 2),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "2",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key: []byte("key3"),
						}},
					}},
					Endorsements: testsig.CreateEndorsementsForThresholdRule([]byte("dummy")),
				},
			},
			{
				Ref: committerpb.NewTxRef("duplicate namespace", 2, 4),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*applicationpb.ReadWrite{{
								Key: []byte("key"),
							}},
						},
						{
							NsId:      committerpb.MetaNamespaceID,
							NsVersion: 0,
							ReadWrites: []*applicationpb.ReadWrite{{
								Key:   []byte("2"),
								Value: nsPolicy,
							}},
						},
						{
							NsId:      "1",
							NsVersion: 0,
						},
					},
					Endorsements: make([]*applicationpb.Endorsements, 3),
				},
			},
			{
				Ref: committerpb.NewTxRef("tx1", 2, 5),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "1",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   []byte("key1"),
							Value: []byte("value1"),
						}},
					}},
					Endorsements: make([]*applicationpb.Endorsements, 1),
				},
			},
		},
	}

	require.NoError(t, env.csStream.Send(block2))

	env.requireStatus(ctx, t, []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx2", 2, 0),
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx3", 2, 1),
		committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "mvcc conflict", 2, 2),
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "duplicate namespace", 2, 4),
		committerpb.NewTxStatus(committerpb.Status_REJECTED_DUPLICATE_TX_ID, "tx1", 2, 5),
	}, []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx1", 1, 0),
	})
}

// statusExistsForNonDuplicateTxID ensures that the given statuses and height
// exist for the corresponding txIDs in the mock VC, excluding any
// duplicate txID statuses.
func statusExistsForNonDuplicateTxID(t *testing.T, vc *mock.VcService, expectedStatuses []*committerpb.TxStatus) {
	t.Helper()
	persistedTxIDs := make([]string, 0, len(expectedStatuses))
	persistedExpectedStatuses := make([]*committerpb.TxStatus, 0, len(expectedStatuses))
	for _, s := range expectedStatuses {
		if s.Status < committerpb.Status_REJECTED_DUPLICATE_TX_ID {
			persistedTxIDs = append(persistedTxIDs, s.Ref.TxId)
			persistedExpectedStatuses = append(persistedExpectedStatuses, s)
		}
	}

	actualStatuses, err := vc.GetTransactionsStatus(t.Context(), &committerpb.TxIDsBatch{
		TxIds: persistedTxIDs,
	})
	require.NoError(t, err)
	test.RequireProtoElementsMatch(t, persistedExpectedStatuses, actualStatuses.Status)
}

func TestCoordinatorStreamFailureWithSidecar(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)

	env.createNamespaces(t, 0, "1")

	blk := &servicepb.CoordinatorBatch{
		Txs: []*servicepb.TxWithRef{
			{
				Ref: committerpb.NewTxRef("tx1", 1, 0),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "1",
						NsVersion: 0,
						BlindWrites: []*applicationpb.Write{{
							Key: []byte("key1"),
						}},
					}},
					Endorsements: testsig.CreateEndorsementsForThresholdRule([]byte("dummy")),
				},
			},
		},
	}
	require.NoError(t, env.csStream.Send(blk))

	env.requireStatus(ctx, t, []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx1", 1, 0),
	}, nil)

	env.streamCancel() // simulate the failure of sidecar

	// only when the stream is inactive, we do not get an error for NoPendingTransactionProcessing.
	require.Eventually(t, func() bool {
		_, err := env.client.NoPendingTransactionProcessing(ctx, nil)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	// simulate the restart of sidecar
	sCtx, sCancel := context.WithTimeout(ctx, 2*time.Minute)
	t.Cleanup(sCancel)
	csStream, err := env.client.BlockProcessing(sCtx)
	require.NoError(t, err)

	env.csStream = csStream
	env.streamCancel = sCancel

	for _, tx := range blk.Txs {
		tx.Ref.BlockNum = 2
	}
	blk.Txs[0].Ref.TxId = "tx2"
	require.NoError(t, env.csStream.Send(blk))
	env.requireStatus(ctx, t, []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx2", 2, 0),
	}, nil)
}

func (e *coordinatorTestEnv) requireStatus(
	ctx context.Context,
	t *testing.T,
	expectedTxStatus, differentPersisted []*committerpb.TxStatus,
) {
	t.Helper()
	test.RequireProtoElementsMatch(t, expectedTxStatus, e.receiveStatus(t, len(expectedTxStatus)))
	txIDs := make([]string, len(expectedTxStatus))
	for i, s := range expectedTxStatus {
		txIDs[i] = s.Ref.TxId
	}
	testapp.EnsurePersistedTxStatus(ctx, t, e.client, txIDs, deduplicateTxStatus(expectedTxStatus, differentPersisted))
}

// deduplicateTxStatus returns a slice of unique-ID tx-statuses.
func deduplicateTxStatus(txStatusBatch ...[]*committerpb.TxStatus) []*committerpb.TxStatus {
	sz := 0
	for _, s := range txStatusBatch {
		sz += len(s)
	}
	ret := make(map[string]*committerpb.TxStatus, sz)
	for _, txStatus := range txStatusBatch {
		for _, s := range txStatus {
			ret[s.Ref.TxId] = s
		}
	}
	return slices.Collect(maps.Values(ret))
}

func (e *coordinatorTestEnv) receiveStatus(t *testing.T, count int) []*committerpb.TxStatus {
	t.Helper()
	status := make([]*committerpb.TxStatus, 0, count)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		txStatus, err := e.csStream.Recv()
		require.NoError(c, err)
		status = append(status, txStatus.Status...)
		require.Len(c, status, count)
	}, time.Minute, 500*time.Millisecond)
	return status
}

func TestConnectionReadyWithTimeout(t *testing.T) {
	t.Parallel()
	randomEndpoint := &connection.Endpoint{Host: "random", Port: 1234}
	c := NewCoordinatorService(&Config{
		Verifier:           *test.NewTLSMultiClientConfig(test.InsecureTLSConfig, randomEndpoint),
		ValidatorCommitter: *test.NewTLSMultiClientConfig(test.InsecureTLSConfig, randomEndpoint),
		DependencyGraph:    &DependencyGraphConfig{},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	require.False(t, c.WaitForReady(ctx))
}

func TestChunkSizeSentForDepGraph(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)

	txPerBlock := 1990
	b, expectedTxsStatus := makeTestBlock(txPerBlock)
	err := env.csStream.Send(b)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return test.GetIntMetricValue(t, env.coordinator.metrics.transactionReceivedTotal) >= txPerBlock
	}, 4*time.Second, 100*time.Millisecond)

	actualTxsStatus := readTxStatus(t, env.csStream, txPerBlock)
	test.RequireProtoElementsMatch(t, expectedTxsStatus, actualTxsStatus)

	// Wait for all transactions to be committed before checking the metric
	// This handles the race condition where restarted verifier may not have processed all txs yet
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		actual := test.GetIntMetricValue(t, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
			committerpb.Status_COMMITTED.String(),
		))
		assert.Equal(c, txPerBlock, actual, "committed transactions count")
	}, 4*time.Second, 100*time.Millisecond)
}

func TestWaitingTxsCountReturnsToZeroForBlockWithRejectedTxs(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)
	env.createNamespaces(t, 0, "1")

	b, expectedTxsStatus := makeTestBlock(1)
	rejectedStatus := committerpb.NewTxStatus(
		committerpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD,
		"rejected",
		0,
		1,
	)
	b.Rejected = []*committerpb.TxStatus{rejectedStatus}
	expectedTxsStatus = append(expectedTxsStatus, rejectedStatus)

	err := env.csStream.Send(b)
	require.NoError(t, err)

	actualTxsStatus := readTxStatus(t, env.csStream, len(expectedTxsStatus))
	test.RequireProtoElementsMatch(t, expectedTxsStatus, actualTxsStatus)
	require.Equal(t, int32(0), env.coordinator.numTxsInProgress.Load())
}

func TestWaitingTxsCount(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startServiceAndOpenStream(ctx, t)

	txPerBlock := 10
	b, expectedTxsStatus := makeTestBlock(txPerBlock)
	success := channel.Make[bool](ctx, 1)
	go func() {
		success.Write(assert.Eventually(t, func() bool {
			return env.coordinator.numTxsInProgress.Load() == int32(2)
		}, 1*time.Minute, 100*time.Millisecond))
	}()

	verifierStreams := mock.RequireStreams(t, env.verifier, 1)
	verifierStreams[0].MockFaultyNodeDropSize.Store(2)
	err := env.csStream.Send(b)
	require.NoError(t, err)
	isSuccess, ok := success.Read()
	require.True(t, ok, "timed out waiting for tx count")
	require.True(t, isSuccess)

	result, err := env.client.NoPendingTransactionProcessing(t.Context(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), ErrActiveStreamPendingTxProcessing.Error())
	require.Nil(t, result)

	env.sigVerifierGrpcServers.ServersStop[0]()
	require.Eventually(t, func() bool {
		return test.CheckServerStopped(t, env.sigVerifierGrpcServers.Configs[0].GRPC.Endpoint.Address())
	}, 4*time.Second, 500*time.Millisecond)

	t.Log("Start server")
	verifierStreams[0].MockFaultyNodeDropSize.Store(0)
	env.sigVerifierGrpcServers = mock.StartMockVerifierServiceFromServerConfig(
		t,
		env.verifier,
		env.sigVerifierGrpcServers.Configs...,
	)

	t.Log("Read status")
	actualTxsStatus := readTxStatus(t, env.csStream, txPerBlock)
	test.RequireProtoElementsMatch(t, expectedTxsStatus, actualTxsStatus)
	test.EventuallyIntMetric(t, txPerBlock, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		committerpb.Status_COMMITTED.String(),
	), 10*time.Second, 100*time.Millisecond)

	env.streamCancel()
	require.Eventually(t, func() bool {
		if !env.coordinator.streamActive.TryLock() {
			return false
		}
		defer env.coordinator.streamActive.Unlock()
		return true
	}, 2*time.Second, 100*time.Millisecond)

	require.Eventually(t, func() bool {
		idle, err := env.client.NoPendingTransactionProcessing(t.Context(), nil)
		if err != nil {
			return false
		}
		return idle.GetValue()
	}, 2*time.Second, 100*time.Millisecond)
}

func readTxStatus(t *testing.T, stream servicepb.Coordinator_BlockProcessingClient, count int) []*committerpb.TxStatus {
	t.Helper()
	actualTxsStatus := make([]*committerpb.TxStatus, 0, count)
	for len(actualTxsStatus) < count {
		txStatus, err := stream.Recv()
		require.NoError(t, err)
		actualTxsStatus = append(actualTxsStatus, txStatus.Status...)
	}
	return actualTxsStatus
}

func makeTestBlock(txPerBlock int) (
	*servicepb.CoordinatorBatch, []*committerpb.TxStatus,
) {
	b := &servicepb.CoordinatorBatch{
		Txs: make([]*servicepb.TxWithRef, txPerBlock),
	}
	expectedTxsStatus := make([]*committerpb.TxStatus, txPerBlock)
	for i := range txPerBlock {
		txID := "tx" + strconv.FormatUint(utils.RandIntN(math.MaxUint64), 16)
		b.Txs[i] = &servicepb.TxWithRef{
			Ref: committerpb.NewTxRef(txID, 0, uint32(i)), //nolint:gosec
			Content: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:      "1",
					NsVersion: 0,
					BlindWrites: []*applicationpb.Write{{
						Key: []byte("key" + strconv.Itoa(i)),
					}},
				}},
				Endorsements: testsig.CreateEndorsementsForThresholdRule([]byte("dummy")),
			},
		}
		//nolint: gosec // int -> uint32.
		expectedTxsStatus[i] = committerpb.NewTxStatus(committerpb.Status_COMMITTED, txID, 0, uint32(i))
	}

	return b, expectedTxsStatus
}

//nolint:ireturn // returning a gRPC client interface is intentional for test purpose.
func createCoordinatorClientWithTLS(
	t *testing.T,
	ep *connection.Endpoint,
	tlsCfg connection.TLSConfig,
) servicepb.CoordinatorClient {
	t.Helper()
	return test.CreateClientWithTLS(t, ep, tlsCfg, servicepb.NewCoordinatorClient)
}
