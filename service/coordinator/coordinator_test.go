/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"maps"
	"math/rand"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/apptest"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type (
	coordinatorTestEnv struct {
		coordinator            *Service
		config                 *Config
		client                 servicepb.CoordinatorClient
		csStream               servicepb.Coordinator_BlockProcessingClient
		streamCancel           context.CancelFunc
		dbEnv                  *vc.DatabaseTestEnv
		verifier               *mock.Verifier
		sigVerifierGrpcServers *test.GrpcServers
	}

	testConfig struct {
		numSigService int
		numVcService  int
		mockVcService bool
	}
)

// TestCoordinatorSecureConnection verifies the Coordinator gRPC server's behavior
// under various client TLS configurations.
func TestCoordinatorSecureConnection(t *testing.T) {
	t.Parallel()
	test.RunSecureConnectionTest(t,
		func(t *testing.T, tlsCfg, _ connection.TLSConfig) test.RPCAttempt {
			t.Helper()
			env := newCoordinatorTestEnv(t, &testConfig{
				numSigService: 1,
				numVcService:  1,
				mockVcService: true,
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
			t.Cleanup(cancel)
			env.startServiceWithCreds(ctx, t, tlsCfg)
			return func(ctx context.Context, t *testing.T, cfg connection.TLSConfig) error {
				t.Helper()
				client := createCoordinatorClientWithTLS(t, &env.coordinator.config.Server.Endpoint, cfg)
				_, err := client.GetNextBlockNumberToCommit(ctx, nil)
				return err
			}
		},
	)
}

func newCoordinatorTestEnv(t *testing.T, tConfig *testConfig) *coordinatorTestEnv {
	t.Helper()
	verifier, svServers := mock.StartMockVerifierService(t, tConfig.numSigService)

	vcServerConfigs := make([]*connection.ServerConfig, 0, tConfig.numVcService)
	var vcsTestEnv *vc.ValidatorAndCommitterServiceTestEnv
	var dbEnv *vc.DatabaseTestEnv

	if !tConfig.mockVcService {
		vcsTestEnv = vc.NewValidatorAndCommitServiceTestEnvWithTLS(t, tConfig.numVcService, test.InsecureTLSConfig)
		for _, c := range vcsTestEnv.Configs {
			vcServerConfigs = append(vcServerConfigs, c.Server)
		}
		dbEnv = vcsTestEnv.GetDBEnv()
	} else {
		_, vcServers := mock.StartMockVCService(t, tConfig.numVcService)
		vcServerConfigs = vcServers.Configs
	}

	c := &Config{
		Verifier:           *test.ServerToMultiClientConfig(svServers.Configs...),
		ValidatorCommitter: *test.ServerToMultiClientConfig(vcServerConfigs...),
		DependencyGraph: &DependencyGraphConfig{
			NumOfLocalDepConstructors: 3,
			WaitingTxsLimit:           10,
		},
		ChannelBufferSizePerGoroutine: 2000,
		Monitoring: monitoring.Config{
			Server: connection.NewLocalHostServer(test.InsecureTLSConfig),
		},
	}

	return &coordinatorTestEnv{
		coordinator:            NewCoordinatorService(c),
		config:                 c,
		dbEnv:                  dbEnv,
		verifier:               verifier,
		sigVerifierGrpcServers: svServers,
	}
}

func (e *coordinatorTestEnv) startInsecureServiceAndOpenStream(ctx context.Context, t *testing.T) {
	t.Helper()
	e.startServiceWithCreds(ctx, t, test.InsecureTLSConfig)
	e.client = createCoordinatorClientWithTLS(t, &e.coordinator.config.Server.Endpoint, test.InsecureTLSConfig)

	sCtx, sCancel := context.WithTimeout(ctx, 5*time.Minute)
	t.Cleanup(sCancel)
	csStream, err := e.client.BlockProcessing(sCtx)
	require.NoError(t, err)

	e.csStream = csStream
	e.streamCancel = sCancel
}

func (e *coordinatorTestEnv) startServiceWithCreds(
	ctx context.Context,
	t *testing.T,
	serverCreds connection.TLSConfig,
) {
	t.Helper()
	cs := e.coordinator
	e.coordinator.config.Server = connection.NewLocalHostServer(serverCreds)

	test.RunServiceAndGrpcForTest(ctx, t, cs, e.coordinator.config.Server)
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
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1, mockVcService: true})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

	env.ensureStreamActive(t)

	stream, err := env.client.BlockProcessing(ctx)
	require.NoError(t, err)
	_, err = stream.Recv()
	require.ErrorContains(t, err, ErrExistingStreamOrConflictingOp.Error())
}

func TestGetNextBlockNumWithActiveStream(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1, mockVcService: true})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

	env.ensureStreamActive(t)

	_, err := env.client.GetNextBlockNumberToCommit(ctx, nil)
	require.NoError(t, err)
}

func TestCoordinatorServiceValidTx(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 2, numVcService: 2, mockVcService: true})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

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

func TestCoordinatorServiceRejectedTx(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 2, numVcService: 2, mockVcService: true})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

	env.createNamespaces(t, 0, "1")

	preMetricsValue := test.GetIntMetricValue(t, env.coordinator.metrics.transactionReceivedTotal)

	err := env.csStream.Send(&servicepb.CoordinatorBatch{
		Rejected: []*committerpb.TxStatus{
			{
				Ref:    committerpb.NewTxRef("rejected", 1, 0),
				Status: committerpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD,
			},
		},
	})
	require.NoError(t, err)
	test.EventuallyIntMetric(
		t, preMetricsValue+1, env.coordinator.metrics.transactionReceivedTotal,
		1*time.Second, 100*time.Millisecond,
	)

	env.requireStatus(ctx, t, []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD, "rejected", 1, 0),
	}, nil)

	test.RequireIntMetricValue(t, 1, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		committerpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD.String(),
	))
	test.RequireIntMetricValue(t, preMetricsValue, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		committerpb.Status_COMMITTED.String(),
	))

	_, err = env.coordinator.SetLastCommittedBlockNumber(ctx, &servicepb.BlockRef{Number: 1})
	require.NoError(t, err)

	nextBlock, err := env.coordinator.GetNextBlockNumberToCommit(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, nextBlock)
	require.Equal(t, uint64(2), nextBlock.Number)
}

func TestCoordinatorServiceDependentOrderedTxs(t *testing.T) {
	t.Parallel()
	// TODO: Use real signature verifier instead of mocks.
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 2, numVcService: 2, mockVcService: false})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

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
							Version: applicationpb.NewVersion(0),
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
							Version: applicationpb.NewVersion(2),
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
							Version: applicationpb.NewVersion(2),
							Value:   []byte("Value of version 3"),
						}},
					}},
				},
			},
		},
	}

	for _, b := range []*servicepb.CoordinatorBatch{b0, b1} {
		for _, tx := range b.Txs {
			tx.Content.Endorsements = sigtest.CreateEndorsementsForThresholdRule([]byte("dummy"))
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

	res := env.dbEnv.FetchKeys(t, utNsID, [][]byte{mainKey, subKey})
	mainValue, ok := res[string(mainKey)]
	require.True(t, ok)
	require.EqualValues(t, 3, mainValue.Version)

	subValue, ok := res[string(subKey)]
	require.True(t, ok)
	require.EqualValues(t, 0, subValue.Version)
}

func TestQueueSize(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 2, numVcService: 2, mockVcService: true})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	go env.coordinator.monitorQueues(ctx)

	q := env.coordinator.queues
	m := env.coordinator.metrics
	q.depGraphToSigVerifierFreeTxs <- dependencygraph.TxNodeBatch{}
	q.sigVerifierToVCServiceValidatedTxs <- dependencygraph.TxNodeBatch{}
	q.vcServiceToDepGraphValidatedTxs <- dependencygraph.TxNodeBatch{}
	q.vcServiceToCoordinatorTxStatus <- &committerpb.TxStatusBatch{}

	require.Eventually(t, func() bool {
		return test.GetIntMetricValue(t, m.sigverifierInputTxBatchQueueSize) == 1 &&
			test.GetIntMetricValue(t, m.sigverifierOutputValidatedTxBatchQueueSize) == 1 &&
			test.GetIntMetricValue(t, m.vcserviceOutputValidatedTxBatchQueueSize) == 1 &&
			test.GetIntMetricValue(t, m.vcserviceOutputTxStatusBatchQueueSize) == 1
	}, 3*time.Second, 500*time.Millisecond)

	<-q.depGraphToSigVerifierFreeTxs
	<-q.sigVerifierToVCServiceValidatedTxs
	<-q.vcServiceToDepGraphValidatedTxs
	<-q.vcServiceToCoordinatorTxStatus

	require.Eventually(t, func() bool {
		return test.GetIntMetricValue(t, m.sigverifierInputTxBatchQueueSize) == 0 &&
			test.GetIntMetricValue(t, m.sigverifierOutputValidatedTxBatchQueueSize) == 0 &&
			test.GetIntMetricValue(t, m.vcserviceOutputValidatedTxBatchQueueSize) == 0 &&
			test.GetIntMetricValue(t, m.vcserviceOutputTxStatusBatchQueueSize) == 0
	}, 3*time.Second, 500*time.Millisecond)
}

func TestCoordinatorRecovery(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1, mockVcService: false})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

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

	vcEnv := vc.NewValidatorAndCommitServiceTestEnvWithTLS(t, 1, test.InsecureTLSConfig, env.dbEnv)
	env.config.ValidatorCommitter = *test.ServerToMultiClientConfig(vcEnv.Configs[0].Server)
	env.coordinator = NewCoordinatorService(env.config)
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

	env.dbEnv.StatusExistsForNonDuplicateTxID(ctx, t, expectedTxStatus)

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
					Endorsements: sigtest.CreateEndorsementsForThresholdRule([]byte("dummy")),
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

func TestCoordinatorStreamFailureWithSidecar(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1, mockVcService: true})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

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
					Endorsements: sigtest.CreateEndorsementsForThresholdRule([]byte("dummy")),
				},
			},
		},
	}
	require.NoError(t, env.csStream.Send(blk))

	env.requireStatus(ctx, t, []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx1", 1, 0),
	}, nil)

	env.streamCancel() // simulate the failure of sidecar

	// only when the stream is inactive, we do not get an error for NumberOfWaitingTransactionsForStatus.
	require.Eventually(t, func() bool {
		_, err := env.client.NumberOfWaitingTransactionsForStatus(ctx, nil)
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
	apptest.EnsurePersistedTxStatus(ctx, t, e.client, txIDs, deduplicateTxStatus(expectedTxStatus, differentPersisted))
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
	c := NewCoordinatorService(fakeConfigForTest(t))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	require.False(t, c.WaitForReady(ctx))
}

func TestChunkSizeSentForDepGraph(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1, mockVcService: true})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

	txPerBlock := 1990
	b, expectedTxsStatus := makeTestBlock(txPerBlock)
	err := env.csStream.Send(b)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return test.GetIntMetricValue(t, env.coordinator.metrics.transactionReceivedTotal) >= txPerBlock
	}, 4*time.Second, 100*time.Millisecond)

	actualTxsStatus := readTxStatus(t, env.csStream, txPerBlock)
	test.RequireProtoElementsMatch(t, expectedTxsStatus, actualTxsStatus)
	test.RequireIntMetricValue(t, txPerBlock, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		committerpb.Status_COMMITTED.String(),
	))
}

func TestWaitingTxsCount(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1, mockVcService: true})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

	txPerBlock := 10
	b, expectedTxsStatus := makeTestBlock(txPerBlock)
	success := channel.Make[bool](ctx, 1)
	go func() {
		success.Write(assert.Eventually(t, func() bool {
			return env.coordinator.numWaitingTxsForStatus.Load() == int32(2)
		}, 1*time.Minute, 100*time.Millisecond))
	}()

	verifierStreams := requireStreams(t, env.verifier, 1)
	verifierStreams[0].MockFaultyNodeDropSize = 2
	err := env.csStream.Send(b)
	require.NoError(t, err)
	isSuccess, ok := success.Read()
	require.True(t, ok, "timed out waiting for tx count")
	require.True(t, isSuccess)

	count, err := env.client.NumberOfWaitingTransactionsForStatus(t.Context(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), ErrActiveStreamWaitingTransactions.Error())
	require.Nil(t, count)

	env.sigVerifierGrpcServers.Servers[0].Stop()
	require.Eventually(t, func() bool {
		return test.CheckServerStopped(t, env.sigVerifierGrpcServers.Configs[0].Endpoint.Address())
	}, 4*time.Second, 500*time.Millisecond)

	verifierStreams[0].MockFaultyNodeDropSize = 0
	env.sigVerifierGrpcServers = mock.StartMockVerifierServiceFromServerConfig(
		t,
		env.verifier,
		env.sigVerifierGrpcServers.Configs...,
	)

	actualTxsStatus := readTxStatus(t, env.csStream, txPerBlock)
	test.RequireProtoElementsMatch(t, expectedTxsStatus, actualTxsStatus)
	test.RequireIntMetricValue(t, txPerBlock, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		committerpb.Status_COMMITTED.String(),
	))

	env.streamCancel()
	require.Eventually(t, func() bool {
		if !env.coordinator.streamActive.TryLock() {
			return false
		}
		defer env.coordinator.streamActive.Unlock()
		return true
	}, 2*time.Second, 100*time.Millisecond)

	require.Eventually(t, func() bool {
		wTxs, err := env.client.NumberOfWaitingTransactionsForStatus(t.Context(), nil)
		if err != nil {
			return false
		}
		return wTxs.GetCount() == 0
	}, 2*time.Second, 100*time.Millisecond)
}

func fakeConfigForTest(t *testing.T) *Config {
	t.Helper()
	randomEndpoint, err := connection.NewEndpoint("random:1234")
	require.NoError(t, err)
	return &Config{
		Server:             connection.NewLocalHostServer(test.InsecureTLSConfig),
		Verifier:           *test.NewTLSMultiClientConfig(test.InsecureTLSConfig, randomEndpoint),
		ValidatorCommitter: *test.NewTLSMultiClientConfig(test.InsecureTLSConfig, randomEndpoint),
		DependencyGraph:    &DependencyGraphConfig{},
		Monitoring: monitoring.Config{
			Server: connection.NewLocalHostServer(test.InsecureTLSConfig),
		},
	}
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
		txID := "tx" + strconv.Itoa(rand.Int())
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
				Endorsements: sigtest.CreateEndorsementsForThresholdRule([]byte("dummy")),
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
