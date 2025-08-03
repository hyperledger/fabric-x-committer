/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protovcservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type validatorAndCommitterServiceTestEnvWithClient struct {
	vcs          []*ValidatorCommitterService
	commonClient protovcservice.ValidationAndCommitServiceClient
	clients      []protovcservice.ValidationAndCommitServiceClient
	streams      []protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamClient
	dbEnv        *DatabaseTestEnv
}

func TestVCSecureConnection(t *testing.T) {
	t.Parallel()
	for _, TLSMode := range test.ServerModes {
		test.RunSecureConnectionTest(t,
			test.SecureConnectionParameters{
				Service:       "validator-committer",
				ServerTLSMode: TLSMode,
				TestCases:     test.BuildTestCases(t, TLSMode),
				ServerStarter: func(t *testing.T, cfg *connection.TLSConfig) connection.Endpoint {
					t.Helper()
					env := newValidatorAndCommitServiceTestEnvWithTLS(t, 1, cfg)
					return env.Configs[0].Server.Endpoint
				},
				ClientStarter: func(t *testing.T, ep *connection.Endpoint, cfg *connection.TLSConfig) test.RequestFunc {
					t.Helper()
					client := createVcClientWithTLS(t, ep, cfg)
					return func(ctx context.Context) error {
						_, err := client.SetupSystemTablesAndNamespaces(ctx, nil)
						return err
					}
				},
				Parallel: true,
			},
		)
	}
}

func newValidatorAndCommitServiceTestEnvWithClient(
	t *testing.T,
	numServices int,
) *validatorAndCommitterServiceTestEnvWithClient {
	t.Helper()
	vcs := NewValidatorAndCommitServiceTestEnv(t, numServices)

	allEndpoints := make([]*connection.Endpoint, len(vcs.Configs))
	for i, c := range vcs.Configs {
		allEndpoints[i] = &c.Server.Endpoint
	}
	commonConn, connErr := connection.Connect(connection.NewInsecureLoadBalancedDialConfig(allEndpoints))
	require.NoError(t, connErr)

	vcsTestEnv := &validatorAndCommitterServiceTestEnvWithClient{
		vcs:          vcs.VCServices,
		commonClient: protovcservice.NewValidationAndCommitServiceClient(commonConn),
		clients:      make([]protovcservice.ValidationAndCommitServiceClient, numServices),
		streams:      make([]protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamClient, numServices),
		dbEnv:        vcs.DBEnv,
	}

	initCtx, initCancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer initCancel()
	_, setupErr := vcsTestEnv.commonClient.SetupSystemTablesAndNamespaces(initCtx, nil)
	require.NoError(t, setupErr)

	for i, c := range vcs.Configs {
		clientConn, err := connection.Connect(connection.NewInsecureDialConfig(&c.Server.Endpoint))
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, clientConn.Close())
		})
		client := protovcservice.NewValidationAndCommitServiceClient(clientConn)

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
	env := newValidatorAndCommitServiceTestEnvWithClient(t, 1)
	p := &protoblocktx.NamespacePolicy{
		Scheme:    "ECDSA",
		PublicKey: []byte("public-key"),
	}
	pBytes, err := proto.Marshal(p)
	require.NoError(t, err)
	configID := "create config"
	configValue := []byte("config")
	txBatch1 := &protovcservice.TransactionBatch{
		Transactions: []*protovcservice.Transaction{{
			ID: configID,
			Namespaces: []*protoblocktx.TxNamespace{{
				NsId:      types.ConfigNamespaceID,
				NsVersion: 0,
				BlindWrites: []*protoblocktx.Write{
					{
						Key:   []byte(types.ConfigKey),
						Value: []byte("config"),
					},
				},
			}},
			BlockNumber: 0,
			TxNum:       0,
		}},
	}

	require.NoError(t, env.streams[0].Send(txBatch1))
	txStatus1, err := env.streams[0].Recv()
	require.NoError(t, err)
	require.NotNil(t, txStatus1)
	require.NotNil(t, txStatus1.Status)

	require.Equal(t,
		types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 0, 0),
		txStatus1.Status[configID],
	)

	ctx, _ := createContext(t)
	tx, err := env.dbEnv.DB.readConfigTX(ctx)
	require.NoError(t, err)
	require.NotNil(t, tx)
	require.Equal(t, configValue, tx.Envelope)

	metaID := "create namespace 1"
	utNsID := "1"
	txBatch2 := &protovcservice.TransactionBatch{
		Transactions: []*protovcservice.Transaction{{
			ID: metaID,
			Namespaces: []*protoblocktx.TxNamespace{{
				NsId:      types.MetaNamespaceID,
				NsVersion: 0,
				ReadWrites: []*protoblocktx.ReadWrite{
					{
						Key:   []byte(utNsID),
						Value: pBytes,
					},
				},
			}},
			BlockNumber: 1,
			TxNum:       0,
		}},
	}
	require.NoError(t, env.streams[0].Send(txBatch2))
	txStatus2, err := env.streams[0].Recv()
	require.NoError(t, err)
	require.NotNil(t, txStatus2)
	require.NotNil(t, txStatus2.Status)

	require.Equal(t,
		types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 1, 0),
		txStatus2.Status[metaID],
	)

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
		env := newValidatorAndCommitServiceTestEnvWithClient(t, 1)
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
		txBatch := &protovcservice.TransactionBatch{
			Transactions: []*protovcservice.Transaction{
				// The following 3 TXs test the blind write path, merging to the update path
				{
					ID: "Blind write without value",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							BlindWrites: []*protoblocktx.Write{
								{
									Key: []byte("blind write without value"),
								},
							},
						},
					},
					BlockNumber: 1,
					TxNum:       1,
				},
				{
					ID: "Blind write with value",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							BlindWrites: []*protoblocktx.Write{
								{
									Key:   []byte("Blind write with value"),
									Value: []byte("value2"),
								},
							},
						},
					},
					BlockNumber: 1,
					TxNum:       2,
				},
				{
					ID: "Blind write update existing key",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							BlindWrites: []*protoblocktx.Write{
								{
									Key:   []byte("Existing key update"),
									Value: []byte("new-value"),
								},
							},
						},
					},
					BlockNumber: 2,
					TxNum:       3,
				},
				// The following 2 TXs test the new key path
				{
					ID: "New key with value",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:   []byte("New key with value"),
									Value: []byte("value3"),
								},
							},
						},
					},
					BlockNumber: 2,
					TxNum:       4,
				},
				{
					ID: "New key no value",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key: []byte("New key no value"),
								},
							},
						},
					},
					BlockNumber: 3,
					TxNum:       5,
				},
				// The following TX tests the update path
				{
					ID: "Existing key",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("Existing key"),
									Value:   []byte("new-value"),
									Version: types.Version(0),
								},
							},
						},
					},
					BlockNumber: 2,
					TxNum:       6,
				},
			},
		}

		require.Zero(t, test.GetIntMetricValue(t, env.vcs[0].metrics.transactionReceivedTotal))

		require.NoError(t, env.streams[0].Send(txBatch))
		txStatus, err := env.streams[0].Recv()
		require.NoError(t, err)

		expectedTxStatus := make(map[string]*protoblocktx.StatusWithHeight)
		txIDs := make([]string, len(txBatch.Transactions))
		for i, tx := range txBatch.Transactions {
			status := types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, tx.BlockNumber, int(tx.TxNum))
			expectedTxStatus[tx.ID] = status
			txIDs[i] = tx.ID
			assert.EqualExportedValuesf(t, status, txStatus.Status[tx.ID], "TX ID: %s", tx.ID)
		}

		test.RequireIntMetricValue(t, len(txBatch.Transactions), env.vcs[0].metrics.transactionReceivedTotal)
		require.EqualExportedValues(t, expectedTxStatus, txStatus.Status)

		env.dbEnv.StatusExistsForNonDuplicateTxID(t, expectedTxStatus)

		ctx, _ := createContext(t)
		test.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)

		txBatch = &protovcservice.TransactionBatch{
			Transactions: []*protovcservice.Transaction{
				{
					ID: "New key 2 no value",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key: []byte("New key 2 no value"),
								},
							},
						},
					},
					BlockNumber: 2,
				},
			},
		}

		require.NoError(t, env.streams[0].Send(txBatch))

		require.Eventually(t, func() bool {
			txStatus, err = env.streams[0].Recv()
			require.NoError(t, err)
			require.Equal(t, protoblocktx.Status_COMMITTED, txStatus.Status["New key 2 no value"].Code)
			return true
		}, env.vcs[0].timeoutForMinTxBatchSize, 500*time.Millisecond)
	})

	t.Run("invalid tx", func(t *testing.T) {
		t.Parallel()
		env := setup()
		txBatch := &protovcservice.TransactionBatch{
			Transactions: []*protovcservice.Transaction{
				{
					ID: "Namespace version mismatch",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 1,
							BlindWrites: []*protoblocktx.Write{
								{
									Key: []byte("blind write without value"),
								},
							},
						},
					},
					BlockNumber: 4,
					TxNum:       1,
				},
				{
					ID: "prelim invalid tx",
					PrelimInvalidTxStatus: &protovcservice.InvalidTxStatus{
						Code: protoblocktx.Status_MALFORMED_DUPLICATE_NAMESPACE,
					},
					BlockNumber: 5,
					TxNum:       2,
				},
				{
					ID: "invalid new writes",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("Existing key"),
									Value:   []byte("new-value"),
									Version: nil,
								},
							},
						},
					},
					BlockNumber: 2,
					TxNum:       6,
				},
				{
					ID:          "Rejected TX",
					BlockNumber: 2,
					TxNum:       7,
					PrelimInvalidTxStatus: &protovcservice.InvalidTxStatus{
						Code: protoblocktx.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD,
					},
				},
			},
		}

		require.NoError(t, env.streams[0].Send(txBatch))
		txStatus, err := env.streams[0].Recv()
		require.NoError(t, err)

		expectedStatus := []protoblocktx.Status{
			protoblocktx.Status_ABORTED_MVCC_CONFLICT,
			protoblocktx.Status_MALFORMED_DUPLICATE_NAMESPACE,
			protoblocktx.Status_ABORTED_MVCC_CONFLICT,
			protoblocktx.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD,
		}

		expectedTxStatus := make(map[string]*protoblocktx.StatusWithHeight, len(txBatch.Transactions))
		txIDs := make([]string, len(txBatch.Transactions))
		for i, tx := range txBatch.Transactions {
			expectedTxStatus[tx.ID] = types.CreateStatusWithHeight(expectedStatus[i], tx.BlockNumber, int(tx.TxNum))
			txIDs = append(txIDs, tx.ID)
		}

		require.Equal(t, expectedTxStatus, txStatus.Status)

		env.dbEnv.StatusExistsForNonDuplicateTxID(t, expectedTxStatus)

		ctx, _ := createContext(t)
		status, err := env.commonClient.GetTransactionsStatus(ctx, &protoblocktx.QueryStatus{TxIDs: txIDs})
		require.NoError(t, err)
		require.Equal(t, expectedTxStatus, status.Status)
	})
}

func TestLastCommittedBlockNumber(t *testing.T) {
	t.Parallel()
	numServices := 3
	env := newValidatorAndCommitServiceTestEnvWithClient(t, numServices)

	ctx, _ := createContext(t)
	for i := range numServices {
		lastCommittedBlock, err := env.clients[i].GetLastCommittedBlockNumber(ctx, nil)
		require.NoError(t, err)
		require.Nil(t, lastCommittedBlock.Block)
	}

	_, err := env.commonClient.SetLastCommittedBlockNumber(ctx, &protoblocktx.BlockInfo{Number: 0})
	require.NoError(t, err)

	for i := range numServices {
		lastCommittedBlock, err := env.clients[i].GetLastCommittedBlockNumber(ctx, nil)
		require.NoError(t, err)
		require.NotNil(t, lastCommittedBlock.Block)
		require.Equal(t, uint64(0), lastCommittedBlock.Block.Number)
	}
}

func TestGRPCStatusCode(t *testing.T) {
	t.Parallel()
	env := newValidatorAndCommitServiceTestEnvWithClient(t, 1)
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
			fn:   func() (any, error) { return c.SetLastCommittedBlockNumber(ctx, &protoblocktx.BlockInfo{Number: 1}) },
		},
		{
			name: "GetLastCommittedBlockNumber returns an internal error",
			fn:   func() (any, error) { return c.GetLastCommittedBlockNumber(ctx, nil) },
		},
		{
			name: "GetPolicies returns an internal error",
			fn:   func() (any, error) { return c.GetNamespacePolicies(ctx, nil) },
		},
		{
			name: "GetTransactionsStatus returns an internal error",
			fn: func() (any, error) {
				return c.GetTransactionsStatus(ctx, &protoblocktx.QueryStatus{TxIDs: []string{"t1"}})
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
	env := newValidatorAndCommitServiceTestEnvWithClient(t, 1)

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
		env := newValidatorAndCommitServiceTestEnvWithClient(t, numServices)

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
		tx             *protovcservice.Transaction
		expectedStatus protoblocktx.Status
	}{
		{
			tx: &protovcservice.Transaction{
				ID: "Blind write with value",
				Namespaces: []*protoblocktx.TxNamespace{
					{
						NsId:      "3",
						NsVersion: 0,
						BlindWrites: []*protoblocktx.Write{
							{
								Key:   []byte("Blind write with value"),
								Value: []byte("value2"),
							},
						},
					},
				},
				BlockNumber: 1,
				TxNum:       2,
			},
			expectedStatus: protoblocktx.Status_COMMITTED,
		},
		{
			tx: &protovcservice.Transaction{
				ID: "New key with value",
				Namespaces: []*protoblocktx.TxNamespace{
					{
						NsId:      "3",
						NsVersion: 0,
						ReadWrites: []*protoblocktx.ReadWrite{
							{
								Key:   []byte("New key with value"),
								Value: []byte("value3"),
							},
						},
					},
				},
				BlockNumber: 2,
				TxNum:       4,
			},
			expectedStatus: protoblocktx.Status_COMMITTED,
		},
		{
			tx: &protovcservice.Transaction{
				ID: "New key no value",
				Namespaces: []*protoblocktx.TxNamespace{
					{
						NsId:      "3",
						NsVersion: 0,
						ReadWrites: []*protoblocktx.ReadWrite{
							{
								Key: []byte("New key no value"),
							},
						},
					},
				},
				BlockNumber: 3,
				TxNum:       5,
			},
			expectedStatus: protoblocktx.Status_COMMITTED,
		},
		{
			tx: &protovcservice.Transaction{
				ID: "invalid sign",
				PrelimInvalidTxStatus: &protovcservice.InvalidTxStatus{
					Code: protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
				},
				BlockNumber: 3,
				TxNum:       6,
			},
			expectedStatus: protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
		},
		{
			tx: &protovcservice.Transaction{
				ID: "duplicate namespace",
				PrelimInvalidTxStatus: &protovcservice.InvalidTxStatus{
					Code: protoblocktx.Status_MALFORMED_DUPLICATE_NAMESPACE,
				},
				BlockNumber: 3,
				TxNum:       7,
			},
			expectedStatus: protoblocktx.Status_MALFORMED_DUPLICATE_NAMESPACE,
		},
		{
			tx: &protovcservice.Transaction{
				ID: "conflict",
				Namespaces: []*protoblocktx.TxNamespace{
					{
						NsId:      "3",
						NsVersion: 0,
						ReadWrites: []*protoblocktx.ReadWrite{
							{
								Key: []byte("Existing key"),
							},
						},
					},
				},
				BlockNumber: 3,
				TxNum:       8,
			},
			expectedStatus: protoblocktx.Status_ABORTED_MVCC_CONFLICT,
		},
	}

	txBatch := &protovcservice.TransactionBatch{}
	expectedTxStatus := make(map[string]*protoblocktx.StatusWithHeight)
	txIDs := make([]string, len(txs))
	for i, t := range txs {
		txBatch.Transactions = append(txBatch.Transactions, t.tx)
		expectedTxStatus[t.tx.ID] = types.CreateStatusWithHeight(t.expectedStatus, t.tx.BlockNumber,
			int(t.tx.TxNum))
		txIDs[i] = t.tx.ID
	}

	t.Run("same transactions submitted again after commit", func(t *testing.T) {
		t.Parallel()
		ctx, env := setup()
		require.NoError(t, env.streams[0].Send(txBatch))
		txStatus, err := env.streams[0].Recv()
		require.NoError(t, err)

		require.Equal(t, expectedTxStatus, txStatus.Status)
		env.dbEnv.StatusExistsForNonDuplicateTxID(t, expectedTxStatus)

		// we are submitting the same three transactions again to all vcservices.
		// We should consistently see the same status.
		for i := range 3 {
			require.NoError(t, env.streams[i].Send(txBatch))
			txStatus, err := env.streams[i].Recv()
			require.NoError(t, err)

			require.Equal(t, expectedTxStatus, txStatus.Status)
			env.dbEnv.StatusExistsForNonDuplicateTxID(t, expectedTxStatus)
		}

		test.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)
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

			require.Equal(t, expectedTxStatus, txStatus.Status)
		}
		env.dbEnv.StatusExistsForNonDuplicateTxID(t, expectedTxStatus)
		test.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)
	})

	t.Run("same transactions submitted again within the minbatchsize", func(t *testing.T) {
		t.Parallel()
		ctx, env := setup()
		txBatchWithDup := &protovcservice.TransactionBatch{}
		txBatchWithDup.Transactions = append(txBatchWithDup.Transactions, txBatch.Transactions...)
		txBatchWithDup.Transactions = append(txBatchWithDup.Transactions, txBatch.Transactions...)
		require.NoError(t, env.streams[0].Send(txBatchWithDup))

		txStatus, err := env.streams[0].Recv()
		require.NoError(t, err)
		require.Equal(t, expectedTxStatus, txStatus.Status)

		env.dbEnv.StatusExistsForNonDuplicateTxID(t, expectedTxStatus)
		test.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)
	})

	t.Run("same duplicate transactions submitted in parallel to all vcservices", func(t *testing.T) {
		t.Parallel()
		ctx, env := setup()
		txBatchWithDup := &protovcservice.TransactionBatch{}
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
			require.Equal(t, expectedTxStatus, txStatus.Status)
		}
		env.dbEnv.StatusExistsForNonDuplicateTxID(t, expectedTxStatus)
		test.EnsurePersistedTxStatus(ctx, t, env.commonClient, txIDs, expectedTxStatus)
	})
}

func createContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx, cancel
}
