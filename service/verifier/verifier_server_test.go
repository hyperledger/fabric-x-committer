/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/common/policydsl"
	"github.com/hyperledger/fabric-x-common/core/config/configtest"
	"github.com/hyperledger/fabric/integration/nwo"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	testTimeout = 3 * time.Second
	fakeTxID    = "fake-id"
)

func TestVerifierSecureConnection(t *testing.T) {
	t.Parallel()
	test.RunSecureConnectionTest(t,
		func(t *testing.T, tlsCfg connection.TLSConfig) test.RPCAttempt {
			t.Helper()
			env := newTestState(t, defaultConfigWithTLS(tlsCfg))
			return func(ctx context.Context, t *testing.T, cfg connection.TLSConfig) error {
				t.Helper()
				client := createVerifierClientWithTLS(t, &env.Service.config.Server.Endpoint, cfg)
				_, err := client.StartStream(ctx)
				return err
			}
		},
	)
}

func TestNoVerificationKeySet(t *testing.T) {
	t.Parallel()
	c := newTestState(t, defaultConfigWithTLS(test.InsecureTLSConfig))

	stream, err := c.Client.StartStream(t.Context())
	require.NoError(t, err)

	err = stream.Send(&protosigverifierservice.Batch{})
	require.NoError(t, err)

	t.Log("We should not receive any results with empty batch")
	_, ok := readStream(t, stream, testTimeout)
	require.False(t, ok)
}

func TestNoInput(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfigWithTLS(test.InsecureTLSConfig))

	stream, _ := c.Client.StartStream(t.Context())

	update, _ := defaultUpdate(t)
	err := stream.Send(&protosigverifierservice.Batch{Update: update})
	require.NoError(t, err)

	_, ok := readStream(t, stream, testTimeout)
	require.False(t, ok)
}

func TestMinimalInput(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfigWithTLS(test.InsecureTLSConfig))

	stream, _ := c.Client.StartStream(t.Context())

	update, signers := defaultUpdate(t)

	tx1 := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*protoblocktx.Write{{
				Key: []byte("0001"),
			}},
		}},
	}
	s, _ := signers[1].SignNs(fakeTxID, tx1, 0)
	tx1.Endorsements = test.AppendToEndorsementSetsForThresholdRule(tx1.Endorsements, s)

	tx2 := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*protoblocktx.Write{{
				Key: []byte("0010"),
			}},
		}},
	}

	s, _ = signers[1].SignNs(fakeTxID, tx2, 0)
	tx2.Endorsements = test.AppendToEndorsementSetsForThresholdRule(tx2.Endorsements, s)

	tx3 := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*protoblocktx.Write{{
				Key: []byte("0011"),
			}},
		}},
	}
	s, _ = signers[1].SignNs(fakeTxID, tx3, 0)
	tx3.Endorsements = test.AppendToEndorsementSetsForThresholdRule(tx3.Endorsements, s)

	err := stream.Send(&protosigverifierservice.Batch{
		Update: update,
		Requests: []*protosigverifierservice.Tx{
			{Ref: types.TxRef(fakeTxID, 1, 1), Tx: tx1},
			{Ref: types.TxRef(fakeTxID, 1, 1), Tx: tx2},
			{Ref: types.TxRef(fakeTxID, 1, 1), Tx: tx3},
		},
	})
	require.NoError(t, err)

	ret, ok := readStream(t, stream, testTimeout)
	require.True(t, ok)
	require.Len(t, ret, 3)
}

func TestSignatureRule(t *testing.T) {
	t.Parallel()
	c := newTestState(t, defaultConfigQuickCutoff())

	stream, err := c.Client.StartStream(t.Context())
	require.NoError(t, err)

	update, _ := defaultUpdate(t)
	err = stream.Send(&protosigverifierservice.Batch{Update: update})
	require.NoError(t, err)

	signingIdentities := make([]*nwo.SigningIdentity, 2)

	for i, org := range []string{"Org1", "Org2"} {
		signingIdentities[i] = &nwo.SigningIdentity{
			CertPath: filepath.Join(configtest.GetDevConfigDir(), "crypto/"+org+"/users/User1@"+org+"/msp",
				"signcerts", "User1@"+org+"-cert.pem"),
			KeyPath: filepath.Join(configtest.GetDevConfigDir(), "crypto/"+org+"/users/User1@"+org+"/msp",
				"keystore", "key.pem"),
			MSPID: org,
		}
	}

	serializedSigningIdentities := make([][]byte, len(signingIdentities))
	for i, si := range signingIdentities {
		serializedIdentity, serr := si.Serialize()
		require.NoError(t, serr)
		serializedSigningIdentities[i] = serializedIdentity
	}

	nsPolicy := &protoblocktx.NamespacePolicy{
		Policy: protoutil.MarshalOrPanic(policydsl.Envelope(policydsl.And(policydsl.SignedBy(0), policydsl.SignedBy(1)),
			serializedSigningIdentities)),
		Type: protoblocktx.PolicyType_SIGNATURE_RULE,
	}

	update = &protosigverifierservice.Update{
		NamespacePolicies: &protoblocktx.NamespacePolicies{
			Policies: []*protoblocktx.PolicyItem{
				policy.MakePolicy(t, "2", nsPolicy),
			},
		},
	}

	tx1 := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      "2",
			NsVersion: 0,
			BlindWrites: []*protoblocktx.Write{{
				Key: []byte("0011"),
			}},
		}},
	}

	data, err := signature.ASN1MarshalTxNamespace(fakeTxID, tx1.Namespaces[0])
	require.NoError(t, err)

	signatures := make([][]byte, len(signingIdentities))
	mspIDs := make([][]byte, len(signingIdentities))
	certsBytes := make([][]byte, len(signingIdentities))
	for i, si := range signingIdentities {
		s, serr := si.Sign(data)
		require.NoError(t, serr)
		signatures[i] = s

		mspIDs[i] = []byte(si.MSPID)
		certBytes, rerr := os.ReadFile(si.CertPath)
		require.NoError(t, rerr)
		certsBytes[i] = certBytes
	}

	tx1.Endorsements = []*protoblocktx.Endorsements{
		test.CreateEndorsementsForSignatureRule(signatures, mspIDs, certsBytes),
	}

	requireTestCase(t, stream, &testCase{
		update: update,
		req: &protosigverifierservice.Tx{
			Ref: types.TxRef(fakeTxID, 1, 1), Tx: tx1,
		},
		expectedStatus: protoblocktx.Status_COMMITTED,
	})

	tx1.Endorsements = []*protoblocktx.Endorsements{
		test.CreateEndorsementsForSignatureRule(signatures[0:1], mspIDs[0:1], certsBytes[0:1]),
	}

	requireTestCase(t, stream, &testCase{
		req: &protosigverifierservice.Tx{
			Ref: types.TxRef(fakeTxID, 1, 1), Tx: tx1,
		},
		expectedStatus: protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
	})
}

func TestBadSignature(t *testing.T) {
	t.Parallel()
	c := newTestState(t, defaultConfigQuickCutoff())

	stream, err := c.Client.StartStream(t.Context())
	require.NoError(t, err)

	update, _ := defaultUpdate(t)
	err = stream.Send(&protosigverifierservice.Batch{Update: update})
	require.NoError(t, err)

	requireTestCase(t, stream, &testCase{
		req: &protosigverifierservice.Tx{
			Ref: types.TxRef(fakeTxID, 1, 0),
			Tx: &protoblocktx.Tx{
				Namespaces: []*protoblocktx.TxNamespace{{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{Key: make([]byte, 0)},
					},
				}},
				Endorsements: test.CreateEndorsementsForThresholdRule([]byte{0}, []byte{1}, []byte{2}),
			},
		},
		expectedStatus: protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
	})
}

func TestUpdatePolicies(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfigQuickCutoff())

	ns1 := "ns1"
	ns2 := "ns2"
	tx := makeTX(ns1, ns2)

	t.Run("invalid update stops stream", func(t *testing.T) {
		t.Parallel()
		stream, err := c.Client.StartStream(t.Context())
		require.NoError(t, err)

		update, _ := defaultUpdate(t)

		ns1Policy, _ := makePolicyItem(t, ns1)
		ns2Policy, _ := makePolicyItem(t, ns2)
		err = stream.Send(&protosigverifierservice.Batch{
			Update: &protosigverifierservice.Update{
				Config: update.Config,
				NamespacePolicies: &protoblocktx.NamespacePolicies{
					Policies: []*protoblocktx.PolicyItem{ns1Policy, ns2Policy},
				},
			},
		})
		require.NoError(t, err)

		// We attempt a bad policies update.
		// We expect no update since one of the given policies are invalid.
		p3, _ := makePolicyItem(t, ns1)
		err = stream.Send(&protosigverifierservice.Batch{
			Update: &protosigverifierservice.Update{
				NamespacePolicies: &protoblocktx.NamespacePolicies{
					Policies: []*protoblocktx.PolicyItem{
						p3,
						policy.MakePolicy(t, ns2, &protoblocktx.NamespacePolicy{
							Type: protoblocktx.PolicyType_THRESHOLD_RULE,
							Policy: protoutil.MarshalOrPanic(&protoblocktx.ThresholdRule{
								PublicKey: []byte("bad-key"),
								Scheme:    signature.Ecdsa,
							}),
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

		update, _ := defaultUpdate(t)

		ns1Policy, ns1Signer := makePolicyItem(t, ns1)
		ns2Policy, _ := makePolicyItem(t, ns2)
		err = stream.Send(&protosigverifierservice.Batch{
			Update: &protosigverifierservice.Update{
				Config: update.Config,
				NamespacePolicies: &protoblocktx.NamespacePolicies{
					Policies: []*protoblocktx.PolicyItem{ns1Policy, ns2Policy},
				},
			},
		})
		require.NoError(t, err)

		ns2PolicyUpdate, ns2Signer := makePolicyItem(t, ns2)
		err = stream.Send(&protosigverifierservice.Batch{
			Update: &protosigverifierservice.Update{
				NamespacePolicies: &protoblocktx.NamespacePolicies{
					Policies: []*protoblocktx.PolicyItem{ns2PolicyUpdate},
				},
			},
		})
		require.NoError(t, err)

		sign(t, tx, ns1Signer, ns2Signer)
		requireTestCase(t, stream, &testCase{
			req: &protosigverifierservice.Tx{
				Ref: types.TxRef(fakeTxID, 1, 1),
				Tx:  tx,
			},
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

	update, _ := defaultUpdate(t)

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
			Config: update.Config,
			NamespacePolicies: &protoblocktx.NamespacePolicies{
				Policies: []*protoblocktx.PolicyItem{uniqueNsPolicy, commonNsPolicy},
			},
		}
		err = stream.Send(&protosigverifierservice.Batch{
			Update: p,
		})
		require.NoError(t, err)
	}

	// The following TX updates all the namespaces.
	// We attempt this TX with each of the attempted policies for the common namespace.
	// One and only one should succeed.
	tx := makeTX(ns...)
	success := 0
	for i := range updateCount {
		sign(t, tx, append(uniqueNsSigners, commonNsSigners[i])...)
		require.NoError(t, stream.Send(&protosigverifierservice.Batch{
			Requests: []*protosigverifierservice.Tx{{
				Ref: types.TxRef(fakeTxID, 0, 0),
				Tx:  tx,
			}},
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
	tx = makeTX(ns[:updateCount]...)
	sign(t, tx, uniqueNsSigners...)
	requireTestCase(t, stream, &testCase{
		req: &protosigverifierservice.Tx{
			Ref: types.TxRef(fakeTxID, 1, 1),
			Tx:  tx,
		},
		expectedStatus: protoblocktx.Status_COMMITTED,
	})
}

type testCase struct {
	update         *protosigverifierservice.Update
	req            *protosigverifierservice.Tx
	expectedStatus protoblocktx.Status
}

func sign(t *testing.T, tx *protoblocktx.Tx, signers ...*sigtest.NsSigner) {
	t.Helper()
	tx.Endorsements = make([]*protoblocktx.Endorsements, len(signers))
	for i, s := range signers {
		s, err := s.SignNs(fakeTxID, tx, i)
		require.NoError(t, err)
		tx.Endorsements[i] = test.CreateEndorsementsForThresholdRule(s)[0]
	}
}

func makeTX(namespaces ...string) *protoblocktx.Tx {
	tx := &protoblocktx.Tx{
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
		Type: protoblocktx.PolicyType_THRESHOLD_RULE,
		Policy: protoutil.MarshalOrPanic(&protoblocktx.ThresholdRule{
			Scheme:    signature.Ecdsa,
			PublicKey: verificationKey,
		}),
	})
	return p, txSigner
}

func requireTestCase(
	t *testing.T,
	stream protosigverifierservice.Verifier_StartStreamClient,
	tt *testCase,
) {
	t.Helper()
	err := stream.Send(&protosigverifierservice.Batch{
		Update:   tt.update,
		Requests: []*protosigverifierservice.Tx{tt.req},
	})
	require.NoError(t, err)

	txStatus, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, txStatus)
	require.Len(t, txStatus.Responses, 1)
	resp := txStatus.Responses[0]
	require.NotNil(t, resp)
	test.RequireProtoEqual(t, tt.req.Ref, resp.Ref)
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
	test.RunServiceAndGrpcForTest(t.Context(), t, service, config.Server)

	return &State{
		Service: service,
		Client:  createVerifierClientWithTLS(t, &config.Server.Endpoint, test.InsecureTLSConfig),
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

func defaultUpdate(t *testing.T) (*protosigverifierservice.Update, []*sigtest.NsSigner) {
	t.Helper()
	factory := sigtest.NewSignatureFactory(signature.Ecdsa)
	nsTxSigningKey, nsTxVerificationKey := factory.NewKeys()
	configBlock, err := workload.CreateDefaultConfigBlock(&workload.ConfigBlock{
		MetaNamespaceVerificationKey: nsTxVerificationKey,
	})
	require.NoError(t, err)
	nsTxSigner, _ := factory.NewSigner(nsTxSigningKey)

	dataTxSigningKey, dataTxVerificationKey := factory.NewKeys()
	dataTxSigner, _ := factory.NewSigner(dataTxSigningKey)
	update := &protosigverifierservice.Update{
		Config: &protoblocktx.ConfigTransaction{
			Envelope: configBlock.Data.Data[0],
		},
		NamespacePolicies: &protoblocktx.NamespacePolicies{
			Policies: []*protoblocktx.PolicyItem{
				policy.MakePolicy(t, "1", &protoblocktx.NamespacePolicy{
					Type: protoblocktx.PolicyType_THRESHOLD_RULE,
					Policy: protoutil.MarshalOrPanic(&protoblocktx.ThresholdRule{
						Scheme:    signature.Ecdsa,
						PublicKey: dataTxVerificationKey,
					}),
				}),
			},
		},
	}
	return update, []*sigtest.NsSigner{nsTxSigner, dataTxSigner}
}

func defaultConfigWithTLS(tlsConfig connection.TLSConfig) *Config {
	return &Config{
		Server: connection.NewLocalHostServerWithTLS(tlsConfig),
		ParallelExecutor: ExecutorConfig{
			BatchSizeCutoff:   3,
			BatchTimeCutoff:   1 * time.Hour,
			Parallelism:       3,
			ChannelBufferSize: 1,
		},
		Monitoring: monitoring.Config{
			Server: connection.NewLocalHostServerWithTLS(test.InsecureTLSConfig),
		},
	}
}

func defaultConfigQuickCutoff() *Config {
	config := defaultConfigWithTLS(test.InsecureTLSConfig)
	config.ParallelExecutor.BatchSizeCutoff = 1
	return config
}

//nolint:ireturn // returning a gRPC client interface is intentional for test purpose.
func createVerifierClientWithTLS(
	t *testing.T,
	ep *connection.Endpoint,
	tlsCfg connection.TLSConfig,
) protosigverifierservice.VerifierClient {
	t.Helper()
	return test.CreateClientWithTLS(t, ep, tlsCfg, protosigverifierservice.NewVerifierClient)
}
