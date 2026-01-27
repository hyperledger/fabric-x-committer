/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/apptest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type validatorAndCommitterServiceTestEnvWithClient struct {
	vcs          []*ValidatorCommitterService
	commonClient servicepb.ValidationAndCommitServiceClient
	clients      []servicepb.ValidationAndCommitServiceClient
	streams      []servicepb.ValidationAndCommitService_StartValidateAndCommitStreamClient
	dbEnv        *DatabaseTestEnv
}

func TestVCSecureConnection(t *testing.T) {
	t.Parallel()
	test.RunSecureConnectionTest(t,
		func(t *testing.T, cfg, _ connection.TLSConfig) test.RPCAttempt {
			t.Helper()
			env := NewValidatorAndCommitServiceTestEnv(t, &TestEnvOpts{ServerCreds: cfg})
			return func(ctx context.Context, t *testing.T, cfg connection.TLSConfig) error {
				t.Helper()
				client := createValidatorAndCommitClientWithTLS(t, &env.Configs[0].Server.Endpoint, cfg)
				_, err := client.SetupSystemTablesAndNamespaces(ctx, nil)
				return err
			}
		},
	)
}

func newValidatorAndCommitServiceTestEnvWithClient(
	t *testing.T,
	opts *TestEnvOpts,
) *validatorAndCommitterServiceTestEnvWithClient {
	t.Helper()
	vcs := NewValidatorAndCommitServiceTestEnv(t, opts)

	numServices := len(vcs.VCServices)
	allEndpoints := make([]*connection.Endpoint, numServices)
	for i, c := range vcs.Configs {
		allEndpoints[i] = &c.Server.Endpoint
	}
	commonConn := test.NewInsecureLoadBalancedConnection(t, allEndpoints)

	vcsTestEnv := &validatorAndCommitterServiceTestEnvWithClient{
		vcs:          vcs.VCServices,
		commonClient: servicepb.NewValidationAndCommitServiceClient(commonConn),
		clients:      make([]servicepb.ValidationAndCommitServiceClient, numServices),
		streams:      make([]servicepb.ValidationAndCommitService_StartValidateAndCommitStreamClient, numServices),
		dbEnv:        vcs.DBEnv,
	}

	initCtx, initCancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer initCancel()
	_, setupErr := vcsTestEnv.commonClient.SetupSystemTablesAndNamespaces(initCtx, nil)
	require.NoError(t, setupErr)

	for i, c := range vcs.Configs {
		client := createValidatorAndCommitClientWithTLS(t, &c.Server.Endpoint, test.InsecureTLSConfig)

		sCtx, sCancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(sCancel)
		vcStream, err := client.StartValidateAndCommitStream(sCtx)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			return vcs.VCServices[i].isStreamActive.Load()
		}, 5*time.Second, 50*time.Millisecond)

		vcsTestEnv.clients[i] = client
		vcsTestEnv.streams[i] = vcStream
	}
	return vcsTestEnv
}

func TestCreateConfigAndTables(t *testing.T) {
	t.Parallel()
	env := newValidatorAndCommitServiceTestEnvWithClient(t, nil)
	p := policy.MakeECDSAThresholdRuleNsPolicy([]byte("publick-key"))
	pBytes, err := proto.Marshal(p)
	require.NoError(t, err)
	configID := "create config"
	configValue := []byte("config")
	txBatch1 := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{{
			Ref: committerpb.NewTxRef(configID, 0, 0),
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:      committerpb.ConfigNamespaceID,
				NsVersion: 0,
				BlindWrites: []*applicationpb.Write{
					{
						Key:   []byte(committerpb.ConfigKey),
						Value: []byte("config"),
					},
				},
			}},
		}},
	}

	require.NoError(t, env.streams[0].Send(txBatch1))
	txStatus1, err := env.streams[0].Recv()
	require.NoError(t, err)
	require.NotNil(t, txStatus1)
	require.NotNil(t, txStatus1.Status)

	expectedConfig := committerpb.NewTxStatus(committerpb.Status_COMMITTED, configID, 0, 0)
	apptest.RequireStatus(t, expectedConfig, txStatus1.Status)

	ctx, _ := createContext(t)
	tx, err := env.dbEnv.DB.readConfigTX(ctx)
	require.NoError(t, err)
	require.NotNil(t, tx)
	require.Equal(t, configValue, tx.Envelope)

	metaID := "create namespace 1"
	utNsID := "1"
	txBatch2 := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{{
			Ref: committerpb.NewTxRef(metaID, 1, 0),
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:      committerpb.MetaNamespaceID,
				NsVersion: 0,
				ReadWrites: []*applicationpb.ReadWrite{
					{
						Key:   []byte(utNsID),
						Value: pBytes,
					},
				},
			}},
		}},
	}
	require.NoError(t, env.streams[0].Send(txBatch2))
	txStatus2, err := env.streams[0].Recv()
	require.NoError(t, err)
	require.NotNil(t, txStatus2)
	require.NotNil(t, txStatus2.Status)

	expectedMeta := committerpb.NewTxStatus(committerpb.Status_COMMITTED, metaID, 1, 0)
	apptest.RequireStatus(t, expectedMeta, txStatus2.Status)

	policies, err := env.dbEnv.DB.readNamespacePolicies(ctx)
	require.NoError(t, err)
	require.NotNil(t, policies)
	require.Len(t, policies.Policies, 1)
	require.NotNil(t, policies.Policies[0])
	require.Equal(t, utNsID, policies.Policies[0].Namespace)
	require.Equal(t, pBytes, policies.Policies[0].Policy)

	// Ensure the table exists.
	rows, err := env.dbEnv.DB.pool.Query(ctx, fmt.Sprintf("select key, value from %s", TableName(utNsID)))
	require.NoError(t, err)
	defer rows.Close()
	keys, values, err := readTwoItems[[]byte, []byte](rows)
	require.NoError(t, err)
	require.Empty(t, keys)
	require.Empty(t, values)
}

func TestValidatorAndCommitterService(t *testing.T) {
	t.Parallel()
	setup := func() *validatorAndCommitterServiceTestEnvWithClient {
		env := newValidatorAndCommitServiceTestEnvWithClient(t, nil)
		env.dbEnv.populateData(t, []string{"1"}, namespaceToWrites{
			"1": &namespaceWrites{
				keys:     [][]byte{[]byte("Existing key"), []byte("Existing key update")},
				values:   [][]byte{[]byte("value"), []byte("value")},
				versions: []uint64{0, 0},
			},
		}, nil, nil)
		return env
	}

	t.Run("all valid txs", func(t *testing.T) {
		t.Parallel()
		env := setup()
		txBatch := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				// The following 3 TXs test the blind write path, merging to the update path
				{
					Ref: committerpb.NewTxRef("Blind write without value", 1, 1),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							BlindWrites: []*applicationpb.Write{
								{
									Key: []byte("blind write without value"),
								},
							},
						},
					},
				},
				{
					Ref: committerpb.NewTxRef("Blind write with value", 1, 2),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							BlindWrites: []*applicationpb.Write{
								{
									Key:   []byte("Blind write with value"),
									Value: []byte("value2"),
								},
							},
						},
					},
				},
				{
					Ref: committerpb.NewTxRef("Blind write update existing key", 2, 3),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							BlindWrites: []*applicationpb.Write{
								{
									Key:   []byte("Existing key update"),
									Value: []byte("new-value"),
								},
							},
						},
					},
				},
				// The following 2 TXs test the new key path
				{
					Ref: committerpb.NewTxRef("New key with value", 2, 4),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*applicationpb.ReadWrite{
								{
									Key:   []byte("New key with value"),
									Value: []byte("value3"),
								},
							},
						},
					},
				},
				{
					Ref: committerpb.NewTxRef("New key no value", 3, 5),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*applicationpb.ReadWrite{
								{
									Key: []byte("New key no value"),
								},
							},
						},
					},
				},
				// The following TX tests the update path
				{
					Ref: committerpb.NewTxRef("Existing key", 2, 6),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*applicationpb.ReadWrite{
								{
									Key:     []byte("Existing key"),
									Value:   []byte("new-value"),
									Version: applicationpb.NewVersion(0),
								},
							},
						},
					},
				},
			},
		}

		require.Zero(t, test.GetIntMetricValue(t, env.vcs[0].metrics.transactionReceivedTotal))

		require.NoError(t, env.streams[0].Send(txBatch))
		txStatus, err := env.streams[0].Recv()
		require.NoError(t, err)

		expectedTxStatus := make([]*committerpb.TxStatus, len(txBatch.Transactions))
		txIDs := make([]string, len(txBatch.Transactions))
		for i, tx := range txBatch.Transactions {
			expectedTxStatus[i] = committerpb.NewTxStatusFromRef(tx.Ref, committerpb.Status_COMMITTED)
			txIDs[i] = tx.Ref.TxId
		}
		test.RequireProtoElementsMatch(t, expectedTxStatus, txStatus.Status)

		test.RequireIntMetricValue(t, len(txBatch.Transactions), env.vcs[0].metrics.transactionReceivedTotal)

		env.dbEnv.StatusExistsForNonDuplicateTxID(t.Context(), t, expectedTxStatus)

		ctx, _ := createContext(t)
		apptest.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)

		noKey2TxRef := committerpb.NewTxRef("New key 2 no value", 2, 0)
		txBatch = &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: noKey2TxRef,
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*applicationpb.ReadWrite{{
								Key: []byte(noKey2TxRef.TxId),
							}},
						},
					},
				},
			},
		}

		require.NoError(t, env.streams[0].Send(txBatch))

		expectedStatus := committerpb.NewTxStatusFromRef(noKey2TxRef, committerpb.Status_COMMITTED)
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			txStatus, err = env.streams[0].Recv()
			require.NoError(t, err)
			apptest.RequireStatus(ct, expectedStatus, txStatus.Status)
		}, env.vcs[0].timeoutForMinTxBatchSize, 500*time.Millisecond)
	})

	t.Run("invalid tx", func(t *testing.T) {
		t.Parallel()
		env := setup()
		txBatch := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: committerpb.NewTxRef("Namespace version mismatch", 4, 1),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 1,
							BlindWrites: []*applicationpb.Write{
								{
									Key: []byte("blind write without value"),
								},
							},
						},
					},
				},
				{
					Ref:                   committerpb.NewTxRef("prelim invalid tx", 5, 2),
					PrelimInvalidTxStatus: invalidStatus(committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE),
				},
				{
					Ref: committerpb.NewTxRef("invalid new writes", 2, 6),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*applicationpb.ReadWrite{
								{
									Key:     []byte("Existing key"),
									Value:   []byte("new-value"),
									Version: nil,
								},
							},
						},
					},
				},
				{
					Ref:                   committerpb.NewTxRef("Rejected TX", 2, 7),
					PrelimInvalidTxStatus: invalidStatus(committerpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD),
				},
			},
		}

		require.NoError(t, env.streams[0].Send(txBatch))
		txStatus, err := env.streams[0].Recv()
		require.NoError(t, err)

		expectedStatus := []committerpb.Status{
			committerpb.Status_ABORTED_MVCC_CONFLICT,
			committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE,
			committerpb.Status_ABORTED_MVCC_CONFLICT,
			committerpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD,
		}

		expectedTxStatus := make([]*committerpb.TxStatus, len(txBatch.Transactions))
		txIDs := make([]string, len(txBatch.Transactions))
		for i, tx := range txBatch.Transactions {
			expectedTxStatus[i] = committerpb.NewTxStatusFromRef(tx.Ref, expectedStatus[i])
			txIDs = append(txIDs, tx.Ref.TxId)
		}

		test.RequireProtoElementsMatch(t, expectedTxStatus, txStatus.Status)

		env.dbEnv.StatusExistsForNonDuplicateTxID(t.Context(), t, expectedTxStatus)

		ctx, _ := createContext(t)
		status, err := env.commonClient.GetTransactionsStatus(ctx, &committerpb.TxIDsBatch{TxIds: txIDs})
		require.NoError(t, err)
		test.RequireProtoElementsMatch(t, expectedTxStatus, status.Status)
	})

	t.Run("config tx returned immediately", func(t *testing.T) {
		t.Parallel()
		env := newValidatorAndCommitServiceTestEnvWithClient(t, &TestEnvOpts{
			ResourceLimits: &ResourceLimitsConfig{
				MaxWorkersForPreparer:             2,
				MaxWorkersForValidator:            2,
				MaxWorkersForCommitter:            2,
				MinTransactionBatchSize:           100,
				TimeoutForMinTransactionBatchSize: 1 * time.Hour,
			},
		})

		configTxBatch := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{{
				Ref: committerpb.NewTxRef("config-tx-immediate", 0, 0),
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: committerpb.ConfigNamespaceID,
					BlindWrites: []*applicationpb.Write{{
						Key:   []byte(committerpb.ConfigKey),
						Value: []byte("config-value"),
					}},
				}},
			}},
		}

		require.NoError(t, env.streams[0].Send(configTxBatch))

		resultCh := make(chan *committerpb.TxStatusBatch, 1)
		var wg sync.WaitGroup
		t.Cleanup(wg.Wait)
		wg.Go(func() {
			txStatus, err := env.streams[0].Recv()
			if err != nil {
				t.Log(err.Error())
				close(resultCh)
				return
			}
			resultCh <- txStatus
		})

		select {
		case txStatus := <-resultCh:
			require.Len(t, txStatus.GetStatus(), 1)
		case <-time.After(5 * time.Second):
			t.Fatal("config tx was not processed immediately - batching delay detected")
		}
	})
}

func TestLastCommittedBlockNumber(t *testing.T) {
	t.Parallel()
	numServices := 3
	env := newValidatorAndCommitServiceTestEnvWithClient(t, &TestEnvOpts{NumServices: numServices})

	ctx, _ := createContext(t)
	for i := range numServices {
		nextBlock, err := env.clients[i].GetNextBlockNumberToCommit(ctx, nil)
		require.NoError(t, err)
		require.NotNil(t, nextBlock)
		require.Equal(t, uint64(0), nextBlock.Number)
	}

	_, err := env.commonClient.SetLastCommittedBlockNumber(ctx, &servicepb.BlockRef{Number: 0})
	require.NoError(t, err)

	for i := range numServices {
		nextBlock, err := env.clients[i].GetNextBlockNumberToCommit(ctx, nil)
		require.NoError(t, err)
		require.NotNil(t, nextBlock)
		require.Equal(t, uint64(1), nextBlock.Number)
	}
}

func TestGRPCStatusCode(t *testing.T) {
	t.Parallel()
	env := newValidatorAndCommitServiceTestEnvWithClient(t, nil)
	c := env.commonClient

	ctx, _ := createContext(t)

	t.Run("GetTransactionsStatus returns an invalid argument error", func(t *testing.T) {
		t.Parallel()
		ret, err := c.GetTransactionsStatus(ctx, nil)
		requireGRPCErrorCode(t, codes.InvalidArgument, err, ret)
	})

	env.vcs[0].db.pool.Close()
	env.vcs[0].db.retry = &connection.RetryProfile{
		InitialInterval: 100 * time.Millisecond,
		MaxInterval:     1 * time.Second,
		MaxElapsedTime:  3 * time.Second,
	}

	testCases := []struct {
		name string
		fn   func() (any, error)
	}{
		{
			name: "SetLastCommittedBlockNumber returns an internal error",
			fn: func() (any, error) {
				return c.SetLastCommittedBlockNumber(ctx, &servicepb.BlockRef{Number: 1})
			},
		},
		{
			name: "GetNextBlockNumberToCommit returns an internal error",
			fn:   func() (any, error) { return c.GetNextBlockNumberToCommit(ctx, nil) },
		},
		{
			name: "GetPolicies returns an internal error",
			fn:   func() (any, error) { return c.GetNamespacePolicies(ctx, nil) },
		},
		{
			name: "GetTransactionsStatus returns an internal error",
			fn: func() (any, error) {
				return c.GetTransactionsStatus(ctx, &committerpb.TxIDsBatch{TxIds: []string{"t1"}})
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ret, err := tc.fn()
			requireGRPCErrorCode(t, codes.Internal, err, ret)
		})
	}
}

func requireGRPCErrorCode(t *testing.T, code codes.Code, err error, ret any) {
	t.Helper()
	require.True(t, grpcerror.HasCode(err, code))
	require.Error(t, err)
	require.Nil(t, ret)
}

func TestVCServiceOneActiveStreamOnly(t *testing.T) {
	t.Parallel()
	env := newValidatorAndCommitServiceTestEnvWithClient(t, nil)

	require.Eventually(t, func() bool {
		return env.vcs[0].isStreamActive.Load()
	}, 20*time.Second, 250*time.Millisecond)

	ctx, _ := createContext(t)
	stream, err := env.commonClient.StartValidateAndCommitStream(ctx)
	require.NoError(t, err)
	_, err = stream.Recv()
	require.ErrorContains(t, err, utils.ErrActiveStream.Error())
}

func TestTransactionResubmission(t *testing.T) {
	t.Parallel()
	setup := func() (context.Context, *validatorAndCommitterServiceTestEnvWithClient) {
		numServices := 3
		env := newValidatorAndCommitServiceTestEnvWithClient(t, &TestEnvOpts{NumServices: numServices})

		env.dbEnv.populateData(t, []string{"3"}, namespaceToWrites{
			"3": &namespaceWrites{
				keys:     [][]byte{[]byte("Existing key")},
				values:   [][]byte{[]byte("value")},
				versions: []uint64{0},
			},
		}, nil, nil)

		ctx, _ := createContext(t)
		return ctx, env
	}

	txs := []struct {
		tx             *servicepb.VcTx
		expectedStatus committerpb.Status
	}{
		{
			tx: &servicepb.VcTx{
				Ref: committerpb.NewTxRef("Blind write with value", 1, 2),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "3",
						NsVersion: 0,
						BlindWrites: []*applicationpb.Write{
							{
								Key:   []byte("Blind write with value"),
								Value: []byte("value2"),
							},
						},
					},
				},
			},
			expectedStatus: committerpb.Status_COMMITTED,
		},
		{
			tx: &servicepb.VcTx{
				Ref: committerpb.NewTxRef("New key with value", 2, 4),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "3",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{
							{
								Key:   []byte("New key with value"),
								Value: []byte("value3"),
							},
						},
					},
				},
			},
			expectedStatus: committerpb.Status_COMMITTED,
		},
		{
			tx: &servicepb.VcTx{
				Ref: committerpb.NewTxRef("New key no value", 3, 5),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "3",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{
							{
								Key: []byte("New key no value"),
							},
						},
					},
				},
			},
			expectedStatus: committerpb.Status_COMMITTED,
		},
		{
			tx: &servicepb.VcTx{
				Ref:                   committerpb.NewTxRef("invalid sign", 3, 6),
				PrelimInvalidTxStatus: invalidStatus(committerpb.Status_ABORTED_SIGNATURE_INVALID),
			},
			expectedStatus: committerpb.Status_ABORTED_SIGNATURE_INVALID,
		},
		{
			tx: &servicepb.VcTx{
				Ref:                   committerpb.NewTxRef("duplicate namespace", 3, 7),
				PrelimInvalidTxStatus: invalidStatus(committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE),
			},
			expectedStatus: committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE,
		},
		{
			tx: &servicepb.VcTx{
				Ref: committerpb.NewTxRef("conflict", 3, 8),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "3",
						NsVersion: 0,
						ReadWrites: []*applicationpb.ReadWrite{
							{
								Key: []byte("Existing key"),
							},
						},
					},
				},
			},
			expectedStatus: committerpb.Status_ABORTED_MVCC_CONFLICT,
		},
	}

	txBatch := &servicepb.VcBatch{}
	expectedTxStatus := make([]*committerpb.TxStatus, len(txs))
	txIDs := make([]string, len(txs))
	for i, t := range txs {
		txBatch.Transactions = append(txBatch.Transactions, t.tx)
		expectedTxStatus[i] = committerpb.NewTxStatusFromRef(t.tx.Ref, t.expectedStatus)
		txIDs[i] = t.tx.Ref.TxId
	}

	t.Run("same transactions submitted again after commit", func(t *testing.T) {
		t.Parallel()
		ctx, env := setup()
		require.NoError(t, env.streams[0].Send(txBatch))
		txStatus, err := env.streams[0].Recv()
		require.NoError(t, err)
		test.RequireProtoElementsMatch(t, expectedTxStatus, txStatus.Status)
		env.dbEnv.StatusExistsForNonDuplicateTxID(ctx, t, expectedTxStatus)

		// we are submitting the same three transactions again to all vcservices.
		// We should consistently see the same status.
		for i := range 3 {
			require.NoError(t, env.streams[i].Send(txBatch))
			curTxStatus, receiveErr := env.streams[i].Recv()
			require.NoError(t, receiveErr)
			test.RequireProtoElementsMatch(t, expectedTxStatus, curTxStatus.Status)
			env.dbEnv.StatusExistsForNonDuplicateTxID(ctx, t, expectedTxStatus)
		}

		apptest.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)
	})

	t.Run("same transactions submitted again while previous submission is not yet committed", func(t *testing.T) {
		t.Parallel()
		ctx, env := setup()
		require.NoError(t, env.streams[0].Send(txBatch))
		require.NoError(t, env.streams[0].Send(txBatch))
		for range 2 {
			// as minbatchsize used for test is 1, we should receive two status batches.
			txStatus, err := env.streams[0].Recv()
			require.NoError(t, err)
			test.RequireProtoElementsMatch(t, expectedTxStatus, txStatus.Status)
		}
		env.dbEnv.StatusExistsForNonDuplicateTxID(ctx, t, expectedTxStatus)
		apptest.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)
	})

	t.Run("same transactions submitted again within the minbatchsize", func(t *testing.T) {
		t.Parallel()
		ctx, env := setup()
		txBatchWithDup := &servicepb.VcBatch{}
		txBatchWithDup.Transactions = append(txBatchWithDup.Transactions, txBatch.Transactions...)
		txBatchWithDup.Transactions = append(txBatchWithDup.Transactions, txBatch.Transactions...)
		require.NoError(t, env.streams[0].Send(txBatchWithDup))

		txStatus, err := env.streams[0].Recv()
		require.NoError(t, err)
		test.RequireProtoElementsMatch(t, expectedTxStatus, txStatus.Status)

		env.dbEnv.StatusExistsForNonDuplicateTxID(ctx, t, expectedTxStatus)
		apptest.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)
	})

	t.Run("same duplicate transactions submitted in parallel to all vcservices", func(t *testing.T) {
		t.Parallel()
		ctx, env := setup()
		txBatchWithDup := &servicepb.VcBatch{}
		for range 10 {
			txBatchWithDup.Transactions = append(txBatchWithDup.Transactions, txBatch.Transactions...)
			txBatchWithDup.Transactions = append(txBatchWithDup.Transactions, txBatch.Transactions...)
		}

		for i := range 3 {
			require.NoError(t, env.streams[i].Send(txBatch))
		}

		for i := range 3 {
			txStatus, err := env.streams[i].Recv()
			require.NoError(t, err)
			test.RequireProtoElementsMatch(t, expectedTxStatus, txStatus.Status)
		}
		env.dbEnv.StatusExistsForNonDuplicateTxID(ctx, t, expectedTxStatus)
		apptest.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)
	})
}

func createContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx, cancel
}

//nolint:ireturn // returning a gRPC client interface is intentional for test purpose.
func createValidatorAndCommitClientWithTLS(
	t *testing.T,
	ep *connection.Endpoint,
	tlsCfg connection.TLSConfig,
) servicepb.ValidationAndCommitServiceClient {
	t.Helper()
	return test.CreateClientWithTLS(t, ep, tlsCfg, servicepb.NewValidationAndCommitServiceClient)
}
