/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const testTimeout = 3 * time.Second

func TestNoVerificationKeySet(t *testing.T) {
	t.Parallel()
	c := newTestState(t, defaultConfig())

	stream, err := c.Client.StartStream(t.Context())
	require.NoError(t, err)

	err = stream.Send(&protosigverifierservice.RequestBatch{})
	require.NoError(t, err)

	t.Log("We should not receive any results with empty batch")
	_, ok := readStream(t, stream, testTimeout)
	require.False(t, ok)
}

func TestNoInput(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfig())

	stream, _ := c.Client.StartStream(t.Context())

	update, _ := defaultUpdate(t)
	err := stream.Send(&protosigverifierservice.RequestBatch{Update: update})
	require.NoError(t, err)

	_, ok := readStream(t, stream, testTimeout)
	require.False(t, ok)
}

func TestMinimalInput(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfig())

	stream, _ := c.Client.StartStream(t.Context())

	update, txSigner := defaultUpdate(t)

	tx1 := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*protoblocktx.Write{{
				Key: []byte("0001"),
			}},
		}},
	}
	s, _ := txSigner.SignNs(tx1, 0)
	tx1.Signatures = append(tx1.Signatures, s)

	tx2 := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*protoblocktx.Write{{
				Key: []byte("0010"),
			}},
		}},
	}

	s, _ = txSigner.SignNs(tx2, 0)
	tx2.Signatures = append(tx2.Signatures, s)

	tx3 := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*protoblocktx.Write{{
				Key: []byte("0011"),
			}},
		}},
	}
	s, _ = txSigner.SignNs(tx3, 0)
	tx3.Signatures = append(tx3.Signatures, s)

	err := stream.Send(&protosigverifierservice.RequestBatch{
		Update: update,
		Requests: []*protosigverifierservice.Request{
			{BlockNum: 1, TxNum: 1, Tx: tx1},
			{BlockNum: 1, TxNum: 2, Tx: tx2},
			{BlockNum: 1, TxNum: 3, Tx: tx3},
		},
	})
	require.NoError(t, err)

	ret, ok := readStream(t, stream, testTimeout)
	require.True(t, ok)
	require.Len(t, ret, 3)
}

func TestBadTxFormat(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfigQuickCutoff())

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	t.Cleanup(cancel)
	stream, _ := c.Client.StartStream(ctx)

	update, _ := defaultUpdate(t)
	err := stream.Send(&protosigverifierservice.RequestBatch{Update: update})
	require.NoError(t, err)

	blockNumber := uint64(1)
	for _, tt := range BadTxFormatTestCases { //nolint:paralleltest
		t.Run(tt.Tx.Id, func(t *testing.T) {
			requireTestCase(t, stream, &testCase{
				blkNum:         blockNumber,
				txNum:          0,
				tx:             tt.Tx,
				expectedStatus: tt.ExpectedStatus,
			})
			blockNumber++
		})
	}
}

func TestUpdatePolicies(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfigQuickCutoff())

	ns1 := "ns1"
	ns2 := "ns2"
	tx := makeTX("update", ns1, ns2)

	t.Run("invalid update stops stream", func(t *testing.T) {
		t.Parallel()
		stream, err := c.Client.StartStream(t.Context())
		require.NoError(t, err)

		ns1Policy, _ := makePolicyItem(t, ns1)
		ns2Policy, _ := makePolicyItem(t, ns2)
		err = stream.Send(&protosigverifierservice.RequestBatch{
			Update: &protosigverifierservice.Update{
				NamespacePolicies: &protoblocktx.NamespacePolicies{
					Policies: []*protoblocktx.PolicyItem{ns1Policy, ns2Policy},
				},
			},
		})
		require.NoError(t, err)

		// We attempt a bad policies update.
		// We expect no update since one of the given policies are invalid.
		p3, _ := makePolicyItem(t, ns1)
		err = stream.Send(&protosigverifierservice.RequestBatch{
			Update: &protosigverifierservice.Update{
				NamespacePolicies: &protoblocktx.NamespacePolicies{
					Policies: []*protoblocktx.PolicyItem{
						p3,
						policy.MakePolicy(t, ns2, &protoblocktx.NamespacePolicy{
							PublicKey: []byte("bad-key"),
							Scheme:    signature.Ecdsa,
						}),
					},
				},
			},
		})
		require.NoError(t, err)

		_, err = stream.Recv()
		require.Error(t, err)
		require.Contains(t, err.Error(), ErrUpdatePolicies.Error())
	})

	t.Run("partial update", func(t *testing.T) {
		t.Parallel()
		stream, err := c.Client.StartStream(t.Context())
		require.NoError(t, err)

		ns1Policy, ns1Signer := makePolicyItem(t, ns1)
		ns2Policy, _ := makePolicyItem(t, ns2)
		err = stream.Send(&protosigverifierservice.RequestBatch{
			Update: &protosigverifierservice.Update{
				NamespacePolicies: &protoblocktx.NamespacePolicies{
					Policies: []*protoblocktx.PolicyItem{ns1Policy, ns2Policy},
				},
			},
		})
		require.NoError(t, err)

		ns2PolicyUpdate, ns2Signer := makePolicyItem(t, ns2)
		err = stream.Send(&protosigverifierservice.RequestBatch{
			Update: &protosigverifierservice.Update{
				NamespacePolicies: &protoblocktx.NamespacePolicies{
					Policies: []*protoblocktx.PolicyItem{ns2PolicyUpdate},
				},
			},
		})
		require.NoError(t, err)

		sign(t, tx, ns1Signer, ns2Signer)
		requireTestCase(t, stream, &testCase{
			blkNum:         1,
			txNum:          1,
			tx:             tx,
			expectedStatus: protoblocktx.Status_COMMITTED,
		})
	})
}

func TestMultipleUpdatePolicies(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfigQuickCutoff())

	ns := make([]string, 101)
	for i := range ns {
		ns[i] = fmt.Sprintf("%d", i)
	}

	stream, err := c.Client.StartStream(t.Context())
	require.NoError(t, err)

	// Each policy update will update a unique namespace, and the common namespace.
	updateCount := len(ns) - 1
	uniqueNsSigners := make([]*sigtest.NsSigner, updateCount)
	commonNsSigners := make([]*sigtest.NsSigner, updateCount)
	for i := range updateCount {
		uniqueNsPolicy, uniqueNsSigner := makePolicyItem(t, ns[i])
		uniqueNsSigners[i] = uniqueNsSigner
		commonNsPolicy, commonNsSigner := makePolicyItem(t, ns[len(ns)-1])
		commonNsSigners[i] = commonNsSigner
		p := &protosigverifierservice.Update{
			NamespacePolicies: &protoblocktx.NamespacePolicies{
				Policies: []*protoblocktx.PolicyItem{uniqueNsPolicy, commonNsPolicy},
			},
		}
		err = stream.Send(&protosigverifierservice.RequestBatch{
			Update: p,
		})
		require.NoError(t, err)
	}

	// The following TX updates all the namespaces.
	// We attempt this TX with each of the attempted policies for the common namespace.
	// One and only one should succeed.
	tx := makeTX("all", ns...)
	success := 0
	for i := range updateCount {
		sign(t, tx, append(uniqueNsSigners, commonNsSigners[i])...)
		require.NoError(t, stream.Send(&protosigverifierservice.RequestBatch{
			Requests: []*protosigverifierservice.Request{
				{
					BlockNum: 0,
					TxNum:    0,
					Tx:       tx,
				},
			},
		}))

		txStatus, err := stream.Recv()
		require.NoError(t, err)
		require.NotNil(t, txStatus)
		require.Len(t, txStatus.Responses, 1)
		if txStatus.Responses[0].Status == protoblocktx.Status_COMMITTED {
			success++
		}
	}
	require.Equal(t, 1, success)

	// The following TX updates all the namespaces but the common one.
	// It must succeed.
	tx = makeTX("all", ns[:updateCount]...)
	sign(t, tx, uniqueNsSigners...)
	requireTestCase(t, stream, &testCase{
		blkNum:         1,
		txNum:          1,
		tx:             tx,
		expectedStatus: protoblocktx.Status_COMMITTED,
	})
}

type testCase struct {
	blkNum         uint64
	txNum          uint64
	tx             *protoblocktx.Tx
	expectedStatus protoblocktx.Status
}

func sign(t *testing.T, tx *protoblocktx.Tx, signers ...*sigtest.NsSigner) {
	t.Helper()
	tx.Signatures = make([][]byte, len(signers))
	for i, s := range signers {
		s, err := s.SignNs(tx, i)
		require.NoError(t, err)
		tx.Signatures[i] = s
	}
}

func makeTX(name string, namespaces ...string) *protoblocktx.Tx {
	tx := &protoblocktx.Tx{
		Id:         name,
		Namespaces: make([]*protoblocktx.TxNamespace, len(namespaces)),
	}
	for i, ns := range namespaces {
		tx.Namespaces[i] = &protoblocktx.TxNamespace{
			NsId:      ns,
			NsVersion: 0,
			BlindWrites: []*protoblocktx.Write{{
				Key: []byte("0001"),
			}},
		}
	}
	return tx
}

func makePolicyItem(t *testing.T, ns string) (*protoblocktx.PolicyItem, *sigtest.NsSigner) {
	t.Helper()
	factory := sigtest.NewSignatureFactory(signature.Ecdsa)
	signingKey, verificationKey := factory.NewKeys()
	txSigner, err := factory.NewSigner(signingKey)
	require.NoError(t, err)
	p := policy.MakePolicy(t, ns, &protoblocktx.NamespacePolicy{
		PublicKey: verificationKey,
		Scheme:    signature.Ecdsa,
	})
	return p, txSigner
}

func requireTestCase(
	t *testing.T,
	stream protosigverifierservice.Verifier_StartStreamClient,
	tt *testCase,
) {
	t.Helper()
	err := stream.Send(&protosigverifierservice.RequestBatch{
		Requests: []*protosigverifierservice.Request{
			{
				BlockNum: tt.blkNum,
				TxNum:    tt.txNum,
				Tx:       tt.tx,
			},
		},
	})
	require.NoError(t, err)

	txStatus, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, txStatus)
	require.Len(t, txStatus.Responses, 1)
	resp := txStatus.Responses[0]
	require.NotNil(t, resp)
	require.Equal(t, tt.blkNum, resp.BlockNum)
	require.Equal(t, tt.txNum, resp.TxNum)
	require.Equal(t, tt.tx.Id, resp.TxId)
	t.Logf(tt.expectedStatus.String(), resp.Status.String())
	require.Equal(t, tt.expectedStatus, resp.Status)
}

// State test state.
type State struct {
	Service *Server
	Client  protosigverifierservice.VerifierClient
}

func newTestState(t *testing.T, config *Config) *State {
	t.Helper()
	service := New(config)
	test.RunServiceAndGrpcForTest(t.Context(), t, service, config.Server, func(grpcServer *grpc.Server) {
		protosigverifierservice.RegisterVerifierServer(grpcServer, service)
	})

	clientConnection, err := connection.Connect(connection.NewInsecureDialConfig(&config.Server.Endpoint))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, clientConnection.Close())
	})
	return &State{
		Service: service,
		Client:  protosigverifierservice.NewVerifierClient(clientConnection),
	}
}

func readStream(
	t *testing.T,
	stream protosigverifierservice.Verifier_StartStreamClient,
	timeout time.Duration,
) ([]*protosigverifierservice.Response, bool) {
	t.Helper()
	outputChan := make(chan []*protosigverifierservice.Response, 1)
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	go func() {
		response, err := stream.Recv()
		if err == nil && response != nil && len(response.Responses) > 0 {
			channel.NewWriter(ctx, outputChan).Write(response.Responses)
		}
	}()
	return channel.NewReader(ctx, outputChan).Read()
}

func defaultUpdate(t *testing.T) (*protosigverifierservice.Update, *sigtest.NsSigner) {
	t.Helper()
	factory := sigtest.NewSignatureFactory(signature.Ecdsa)
	signingKey, verificationKey := factory.NewKeys()
	txSigner, _ := factory.NewSigner(signingKey)
	update := &protosigverifierservice.Update{
		NamespacePolicies: &protoblocktx.NamespacePolicies{
			Policies: []*protoblocktx.PolicyItem{
				policy.MakePolicy(t, "1", &protoblocktx.NamespacePolicy{
					PublicKey: verificationKey,
					Scheme:    signature.Ecdsa,
				}),
			},
		},
	}
	return update, txSigner
}

func defaultConfig() *Config {
	return &Config{
		Server: connection.NewLocalHostServer(),
		ParallelExecutor: ExecutorConfig{
			BatchSizeCutoff:   3,
			BatchTimeCutoff:   1 * time.Hour,
			Parallelism:       3,
			ChannelBufferSize: 1,
		},
		Monitoring: monitoring.Config{
			Server: connection.NewLocalHostServer(),
		},
	}
}

func defaultConfigQuickCutoff() *Config {
	config := defaultConfig()
	config.ParallelExecutor.BatchSizeCutoff = 1
	return config
}
