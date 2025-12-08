/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"maps"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type (
	coordinatorTestEnv struct {
		coordinator            *Service
		config                 *Config
		client                 protocoordinatorservice.CoordinatorClient
		csStream               protocoordinatorservice.Coordinator_BlockProcessingClient
		streamCancel           context.CancelFunc
		dbEnv                  *vc.DatabaseTestEnv
		sigVerifiers           []*mock.SigVerifier
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
		func(t *testing.T, tlsCfg connection.TLSConfig) test.RPCAttempt {
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
	svs, svServers := mock.StartMockSVService(t, tConfig.numSigService)

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
			Server: connection.NewLocalHostServerWithTLS(test.InsecureTLSConfig),
		},
	}

	return &coordinatorTestEnv{
		coordinator:            NewCoordinatorService(c),
		config:                 c,
		dbEnv:                  dbEnv,
		sigVerifiers:           svs,
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
	e.coordinator.config.Server = connection.NewLocalHostServerWithTLS(serverCreds)

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
	blk := &protocoordinatorservice.CoordinatorBatch{}
	blk.Txs = append(blk.Txs, &protocoordinatorservice.CoordinatorTx{
		Ref: committerpb.TxRef(uuid.NewString(), blockNum, 0),
		Content: &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId: committerpb.ConfigNamespaceID,
				ReadWrites: []*applicationpb.ReadWrite{{
					Key:   []byte(committerpb.ConfigKey),
					Value: pBytes,
				}},
			}},
		},
	})
	for i, nsID := range nsIDs {
		blk.Txs = append(blk.Txs, &protocoordinatorservice.CoordinatorTx{
			Ref: committerpb.TxRef(uuid.NewString(), blockNum, uint32(i+1)), //nolint:gosec // int -> uint32.
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
	for _, tx := range blk.Txs {
		// The mock verifier verifies that len(tx.Namespace)==len(tx.Signatures)
		tx.Content.Endorsements = make([]*applicationpb.Endorsements, len(tx.Content.Namespaces))
	}

	err = e.csStream.Send(blk)
	require.NoError(t, err)
	status := make(map[string]*applicationpb.StatusWithHeight)
	require.Eventually(t, func() bool {
		txStatus, err := e.csStream.Recv()
		require.NoError(t, err)
		require.NotNil(t, txStatus)
		require.NotNil(t, txStatus.Status)
		maps.Insert(status, maps.All(txStatus.Status))
		return len(status) == len(nsIDs)+1
	}, 2*time.Minute, 10*time.Millisecond)

	for _, s := range status {
		require.Equal(t, applicationpb.Status_COMMITTED.String(), s.Code.String())
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
	err = env.csStream.Send(&protocoordinatorservice.CoordinatorBatch{
		Txs: []*protocoordinatorservice.CoordinatorTx{
			{
				Ref: committerpb.TxRef("tx1", 1, 0),
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

	env.requireStatus(ctx, t, map[string]*applicationpb.StatusWithHeight{
		"tx1": {Code: applicationpb.Status_COMMITTED, BlockNumber: 1},
	}, nil)

	test.RequireIntMetricValue(t, preMetricsValue+1, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		applicationpb.Status_COMMITTED.String(),
	))

	_, err = env.coordinator.SetLastCommittedBlockNumber(ctx, &applicationpb.BlockInfo{Number: 1})
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

	err := env.csStream.Send(&protocoordinatorservice.CoordinatorBatch{
		Rejected: []*protocoordinatorservice.TxStatusInfo{
			{
				Ref:    committerpb.TxRef("rejected", 1, 0),
				Status: applicationpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD,
			},
		},
	})
	require.NoError(t, err)
	test.EventuallyIntMetric(
		t, preMetricsValue+1, env.coordinator.metrics.transactionReceivedTotal,
		1*time.Second, 100*time.Millisecond,
	)

	env.requireStatus(ctx, t, map[string]*applicationpb.StatusWithHeight{
		"rejected": {Code: applicationpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD, BlockNumber: 1},
	}, nil)

	test.RequireIntMetricValue(t, 1, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		applicationpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD.String(),
	))
	test.RequireIntMetricValue(t, preMetricsValue, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		applicationpb.Status_COMMITTED.String(),
	))

	_, err = env.coordinator.SetLastCommittedBlockNumber(ctx, &applicationpb.BlockInfo{Number: 1})
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

	// We send a block with a series of TXs with apparent conflicts, but all should be committed successfully if
	// executed serially.
	b1 := &protocoordinatorservice.CoordinatorBatch{
		Txs: []*protocoordinatorservice.CoordinatorTx{
			{
				Ref: committerpb.TxRef("config TX", 0, 0),
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
				Ref: committerpb.TxRef("create namespace 1", 0, 1),
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
				Ref: committerpb.TxRef("create main key (read-write version 0)", 0, 2),
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
				Ref: committerpb.TxRef("update main key (read-write version 1)", 0, 3),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      utNsID,
						NsVersion: utNsVersion,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:     mainKey,
							Value:   []byte("value of version 1"),
							Version: committerpb.Version(0),
						}},
					}},
				},
			},
			{
				Ref: committerpb.TxRef("update main key (blind-write version 2)", 0, 4),
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
				Ref: committerpb.TxRef("read main key, create sub key (read version 2, read-write version 0)", 0, 5),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      utNsID,
						NsVersion: utNsVersion,
						ReadsOnly: []*applicationpb.Read{{
							Key:     mainKey,
							Version: committerpb.Version(2),
						}},
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:   subKey,
							Value: []byte("Sub value of version 0"),
						}},
					}},
				},
			},
			{
				Ref: committerpb.TxRef("update main key (read-write version 3)", 0, 6),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      utNsID,
						NsVersion: utNsVersion,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key:     mainKey,
							Version: committerpb.Version(2),
							Value:   []byte("Value of version 3"),
						}},
					}},
				},
			},
		},
	}
	for _, tx := range b1.Txs {
		tx.Content.Endorsements = test.CreateEndorsementsForThresholdRule([]byte("dummy"))
	}

	expectedReceived := test.GetIntMetricValue(t, env.coordinator.metrics.transactionReceivedTotal) + len(b1.Txs)

	require.NoError(t, env.csStream.Send(b1))
	require.Eventually(t, func() bool {
		return test.GetIntMetricValue(t, env.coordinator.metrics.transactionReceivedTotal) >= expectedReceived
	}, time.Minute, 500*time.Millisecond)

	status := env.receiveStatus(t, len(b1.Txs))
	for txID, txStatus := range status {
		require.Equal(t, applicationpb.Status_COMMITTED, txStatus.Code, txID)
	}
	test.RequireIntMetricValue(t, expectedReceived, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		applicationpb.Status_COMMITTED.String(),
	))

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
	q.vcServiceToCoordinatorTxStatus <- &applicationpb.TransactionsStatus{}

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

	err := env.csStream.Send(&protocoordinatorservice.CoordinatorBatch{
		Txs: []*protocoordinatorservice.CoordinatorTx{{
			Ref: committerpb.TxRef("tx1", 1, 0),
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

	env.requireStatus(ctx, t, map[string]*applicationpb.StatusWithHeight{
		"tx1": {Code: applicationpb.Status_COMMITTED, BlockNumber: 1},
	}, nil)

	_, err = env.client.SetLastCommittedBlockNumber(ctx, &applicationpb.BlockInfo{Number: 1})
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
	block2 := &protocoordinatorservice.CoordinatorBatch{
		Txs: []*protocoordinatorservice.CoordinatorTx{
			{
				Ref: committerpb.TxRef("tx2", 2, 0),
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
				Ref: committerpb.TxRef("mvcc conflict", 2, 2),
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
				Ref: committerpb.TxRef("tx1", 2, 5),
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

	expectedTxStatus := map[string]*applicationpb.StatusWithHeight{
		"tx2":           servicepb.NewStatusWithHeight(applicationpb.Status_COMMITTED, 2, 0),
		"mvcc conflict": servicepb.NewStatusWithHeight(applicationpb.Status_ABORTED_MVCC_CONFLICT, 2, 2),
		"tx1":           servicepb.NewStatusWithHeight(applicationpb.Status_REJECTED_DUPLICATE_TX_ID, 2, 5),
	}
	env.requireStatus(ctx, t, expectedTxStatus, map[string]*applicationpb.StatusWithHeight{
		"tx1": servicepb.NewStatusWithHeight(applicationpb.Status_COMMITTED, 1, 0),
	})

	cancel()

	vcEnv := vc.NewValidatorAndCommitServiceTestEnvWithTLS(t, 1, test.InsecureTLSConfig, env.dbEnv)
	env.config.ValidatorCommitter = *test.ServerToMultiClientConfig(vcEnv.Configs[0].Server)
	env.coordinator = NewCoordinatorService(env.config)
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

	env.dbEnv.StatusExistsForNonDuplicateTxID(t, expectedTxStatus)

	// Now, we are sending the full block 2.
	block2 = &protocoordinatorservice.CoordinatorBatch{
		Txs: []*protocoordinatorservice.CoordinatorTx{
			{
				Ref: committerpb.TxRef("tx2", 2, 0),
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
				Ref: committerpb.TxRef("tx3", 2, 1),
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
				Ref: committerpb.TxRef("mvcc conflict", 2, 2),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "2",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{{
							Key: []byte("key3"),
						}},
					}},
					Endorsements: test.CreateEndorsementsForThresholdRule([]byte("dummy")),
				},
			},
			{
				Ref: committerpb.TxRef("duplicate namespace", 2, 4),
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
				Ref: committerpb.TxRef("tx1", 2, 5),
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

	env.requireStatus(ctx, t, map[string]*applicationpb.StatusWithHeight{
		"tx2":                 servicepb.NewStatusWithHeight(applicationpb.Status_COMMITTED, 2, 0),
		"tx3":                 servicepb.NewStatusWithHeight(applicationpb.Status_COMMITTED, 2, 1),
		"mvcc conflict":       servicepb.NewStatusWithHeight(applicationpb.Status_ABORTED_MVCC_CONFLICT, 2, 2),
		"duplicate namespace": servicepb.NewStatusWithHeight(applicationpb.Status_COMMITTED, 2, 4),
		"tx1":                 servicepb.NewStatusWithHeight(applicationpb.Status_REJECTED_DUPLICATE_TX_ID, 2, 5),
	}, map[string]*applicationpb.StatusWithHeight{
		"tx1": servicepb.NewStatusWithHeight(applicationpb.Status_COMMITTED, 1, 0),
	})
}

func TestCoordinatorStreamFailureWithSidecar(t *testing.T) {
	t.Parallel()
	env := newCoordinatorTestEnv(t, &testConfig{numSigService: 1, numVcService: 1, mockVcService: true})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startInsecureServiceAndOpenStream(ctx, t)

	env.createNamespaces(t, 0, "1")

	blk := &protocoordinatorservice.CoordinatorBatch{
		Txs: []*protocoordinatorservice.CoordinatorTx{
			{
				Ref: committerpb.TxRef("tx1", 1, 0),
				Content: &applicationpb.Tx{
					Namespaces: []*applicationpb.TxNamespace{{
						NsId:      "1",
						NsVersion: 0,
						BlindWrites: []*applicationpb.Write{{
							Key: []byte("key1"),
						}},
					}},
					Endorsements: test.CreateEndorsementsForThresholdRule([]byte("dummy")),
				},
			},
		},
	}
	require.NoError(t, env.csStream.Send(blk))

	env.requireStatus(ctx, t, map[string]*applicationpb.StatusWithHeight{
		"tx1": {Code: applicationpb.Status_COMMITTED, BlockNumber: 1},
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
	env.requireStatus(ctx, t, map[string]*applicationpb.StatusWithHeight{
		"tx2": {Code: applicationpb.Status_COMMITTED, BlockNumber: 2},
	}, nil)
}

func (e *coordinatorTestEnv) requireStatus(
	ctx context.Context,
	t *testing.T,
	expectedTxStatus, differentPersisted map[string]*applicationpb.StatusWithHeight,
) {
	t.Helper()
	require.EqualExportedValues(t, expectedTxStatus, e.receiveStatus(t, len(expectedTxStatus)))
	var txIDs []string //nolint:prealloc
	for txID := range expectedTxStatus {
		txIDs = append(txIDs, txID)
	}

	maps.Copy(expectedTxStatus, differentPersisted)
	test.EnsurePersistedTxStatus(ctx, t, e.client, txIDs, expectedTxStatus)
}

func (e *coordinatorTestEnv) receiveStatus(t *testing.T, count int) map[string]*applicationpb.StatusWithHeight {
	t.Helper()
	status := make(map[string]*applicationpb.StatusWithHeight)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		txStatus, err := e.csStream.Recv()
		require.NoError(c, err)
		maps.Insert(status, maps.All(txStatus.Status))
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

	actualTxsStatus := make(map[string]*applicationpb.StatusWithHeight)
	for len(actualTxsStatus) < txPerBlock {
		txStatus, err := env.csStream.Recv()
		require.NoError(t, err)
		maps.Copy(actualTxsStatus, txStatus.Status)
	}

	require.Equal(t, expectedTxsStatus, actualTxsStatus)
	test.RequireIntMetricValue(t, txPerBlock, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		applicationpb.Status_COMMITTED.String(),
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

	env.sigVerifiers[0].MockFaultyNodeDropSize = 2
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

	env.sigVerifiers[0].MockFaultyNodeDropSize = 0
	env.sigVerifierGrpcServers = mock.StartMockSVServiceFromListWithConfig(
		t,
		env.sigVerifiers,
		env.sigVerifierGrpcServers.Configs,
	)

	actualTxsStatus := make(map[string]*applicationpb.StatusWithHeight)
	for len(actualTxsStatus) < txPerBlock {
		txStatus, err := env.csStream.Recv()
		require.NoError(t, err)
		maps.Copy(actualTxsStatus, txStatus.Status)
	}

	require.Equal(t, expectedTxsStatus, actualTxsStatus)
	test.RequireIntMetricValue(t, txPerBlock, env.coordinator.metrics.transactionCommittedTotal.WithLabelValues(
		applicationpb.Status_COMMITTED.String(),
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
		Server:             connection.NewLocalHostServerWithTLS(test.InsecureTLSConfig),
		Verifier:           *test.NewTLSMultiClientConfig(test.InsecureTLSConfig, randomEndpoint),
		ValidatorCommitter: *test.NewTLSMultiClientConfig(test.InsecureTLSConfig, randomEndpoint),
		DependencyGraph:    &DependencyGraphConfig{},
		Monitoring: monitoring.Config{
			Server: connection.NewLocalHostServerWithTLS(test.InsecureTLSConfig),
		},
	}
}

func makeTestBlock(txPerBlock int) (
	*protocoordinatorservice.CoordinatorBatch, map[string]*applicationpb.StatusWithHeight,
) {
	b := &protocoordinatorservice.CoordinatorBatch{
		Txs: make([]*protocoordinatorservice.CoordinatorTx, txPerBlock),
	}
	expectedTxsStatus := make(map[string]*applicationpb.StatusWithHeight)
	for i := range txPerBlock {
		txID := "tx" + strconv.Itoa(rand.Int())
		b.Txs[i] = &protocoordinatorservice.CoordinatorTx{
			Ref: committerpb.TxRef(txID, 0, uint32(i)), //nolint:gosec
			Content: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:      "1",
					NsVersion: 0,
					BlindWrites: []*applicationpb.Write{{
						Key: []byte("key" + strconv.Itoa(i)),
					}},
				}},
				Endorsements: test.CreateEndorsementsForThresholdRule([]byte("dummy")),
			},
		}
		//nolint: gosec // int -> uint32.
		expectedTxsStatus[txID] = servicepb.NewStatusWithHeight(applicationpb.Status_COMMITTED, 0, uint32(i))
	}

	return b, expectedTxsStatus
}

//nolint:ireturn // returning a gRPC client interface is intentional for test purpose.
func createCoordinatorClientWithTLS(
	t *testing.T,
	ep *connection.Endpoint,
	tlsCfg connection.TLSConfig,
) protocoordinatorservice.CoordinatorClient {
	t.Helper()
	return test.CreateClientWithTLS(t, ep, tlsCfg, protocoordinatorservice.NewCoordinatorClient)
}
