/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/common/policydsl"
	commonmsp "github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
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

type cryptoParameters struct {
	cryptoPath     string
	update         *servicepb.VerifierUpdates
	metaTxEndorser *sigtest.NsEndorser
	dataTxEndorser *sigtest.NsEndorser
}

func TestVerifierSecureConnection(t *testing.T) {
	t.Parallel()
	test.RunSecureConnectionTest(t,
		func(t *testing.T, tlsCfg, _ connection.TLSConfig) test.RPCAttempt {
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

	err = stream.Send(&servicepb.VerifierBatch{})
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

	cp := defaultCryptoParameters(t)
	err := stream.Send(&servicepb.VerifierBatch{Update: cp.update})
	require.NoError(t, err)

	_, ok := readStream(t, stream, testTimeout)
	require.False(t, ok)
}

func TestMinimalInput(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfigWithTLS(test.InsecureTLSConfig))

	stream, _ := c.Client.StartStream(t.Context())

	cp := defaultCryptoParameters(t)

	tx1 := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{{
				Key: []byte("0001"),
			}},
		}},
	}
	s, _ := cp.dataTxEndorser.EndorseTxNs(fakeTxID, tx1, 0)
	tx1.Endorsements = append(tx1.Endorsements, s)

	tx2 := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{{
				Key: []byte("0010"),
			}},
		}},
	}

	s, _ = cp.dataTxEndorser.EndorseTxNs(fakeTxID, tx2, 0)
	tx2.Endorsements = append(tx2.Endorsements, s)

	tx3 := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      committerpb.MetaNamespaceID,
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{{
				Key: []byte("0011"),
			}},
		}},
	}
	s, _ = cp.metaTxEndorser.EndorseTxNs(fakeTxID, tx3, 0)
	tx3.Endorsements = append(tx3.Endorsements, s)

	err := stream.Send(&servicepb.VerifierBatch{
		Update: cp.update,
		Requests: []*servicepb.TxWithRef{
			{Ref: committerpb.NewTxRef(fakeTxID, 1, 1), Content: tx1},
			{Ref: committerpb.NewTxRef(fakeTxID, 1, 1), Content: tx2},
			{Ref: committerpb.NewTxRef(fakeTxID, 1, 1), Content: tx3},
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

	cp := defaultCryptoParameters(t)
	err = stream.Send(&servicepb.VerifierBatch{Update: cp.update})
	require.NoError(t, err)

	mspDirs := make([]commonmsp.DirLoadParameters, 2)
	peerOrgPath := filepath.Join(cp.cryptoPath, cryptogen.PeerOrganizationsDir)
	for i, org := range []string{"peer-org-0", "peer-org-1"} {
		mspDirs[i].MspName = org
		clientName := "client@" + org + ".com"
		mspDirs[i].MspDir = filepath.Join(peerOrgPath, org, "users", clientName, "msp")
	}

	signingIdentities, err := sigtest.GetSigningIdentities(mspDirs...)
	require.NoError(t, err)
	serializedSigningIdentities := make([][]byte, len(signingIdentities))
	for i, si := range signingIdentities {
		siBytes, serErr := si.Serialize()
		require.NoError(t, serErr)
		serializedSigningIdentities[i] = siBytes
	}

	nsPolicy := &applicationpb.NamespacePolicy{
		Rule: &applicationpb.NamespacePolicy_MspRule{
			MspRule: protoutil.MarshalOrPanic(
				policydsl.Envelope(policydsl.And(policydsl.SignedBy(0), policydsl.SignedBy(1)),
					serializedSigningIdentities)),
		},
	}

	update := &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{
				policy.MakePolicy(t, "2", nsPolicy),
			},
		},
	}

	tx1 := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      "2",
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{{
				Key: []byte("0011"),
			}},
		}},
	}

	data, err := tx1.Namespaces[0].ASN1Marshal(fakeTxID)
	require.NoError(t, err)

	for _, certType := range []int{test.CreatorCertificate, test.CreatorID} {
		signer, signerErr := sigtest.NewNsEndorserFromMsp(certType, mspDirs...)
		require.NoError(t, signerErr)
		sig, signerErr := signer.Endorse(data)
		require.NoError(t, signerErr)
		tx1.Endorsements = []*applicationpb.Endorsements{sig}

		requireTestCase(t, stream, &testCase{
			update: update,
			req: &servicepb.TxWithRef{
				Ref: committerpb.NewTxRef(fakeTxID, 1, 1), Content: tx1,
			},
			expectedStatus: committerpb.Status_COMMITTED,
		})
	}

	// Update the config block to have SampleFabricX profile instead of
	// the default TwoOrgsSampleFabricX.
	_, metaTxVerificationKey := sigtest.NewKeyPair(signature.Ecdsa)
	configBlock, err := workload.CreateDefaultConfigBlock(&workload.ConfigBlock{
		MetaNamespaceVerificationKey: metaTxVerificationKey,
	})
	require.NoError(t, err)
	update = &servicepb.VerifierUpdates{
		Config: &applicationpb.ConfigTransaction{
			Envelope: configBlock.Data.Data[0],
		},
	}

	requireTestCase(t, stream, &testCase{
		update: update,
		req: &servicepb.TxWithRef{
			Ref: committerpb.NewTxRef(fakeTxID, 1, 1), Content: tx1,
		},
		expectedStatus: committerpb.Status_ABORTED_SIGNATURE_INVALID,
	})
}

func TestBadSignature(t *testing.T) {
	t.Parallel()
	c := newTestState(t, defaultConfigQuickCutoff())

	stream, err := c.Client.StartStream(t.Context())
	require.NoError(t, err)

	cp := defaultCryptoParameters(t)
	err = stream.Send(&servicepb.VerifierBatch{Update: cp.update})
	require.NoError(t, err)

	requireTestCase(t, stream, &testCase{
		req: &servicepb.TxWithRef{
			Ref: committerpb.NewTxRef(fakeTxID, 1, 0),
			Content: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*applicationpb.ReadWrite{
						{Key: make([]byte, 0)},
					},
				}},
				Endorsements: sigtest.CreateEndorsementsForThresholdRule([]byte{0}, []byte{1}, []byte{2}),
			},
		},
		expectedStatus: committerpb.Status_ABORTED_SIGNATURE_INVALID,
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

		cp := defaultCryptoParameters(t)

		ns1Policy, _ := makePolicyItem(t, ns1)
		ns2Policy, _ := makePolicyItem(t, ns2)
		err = stream.Send(&servicepb.VerifierBatch{
			Update: &servicepb.VerifierUpdates{
				Config: cp.update.Config,
				NamespacePolicies: &applicationpb.NamespacePolicies{
					Policies: []*applicationpb.PolicyItem{ns1Policy, ns2Policy},
				},
			},
		})
		require.NoError(t, err)

		// We attempt a bad policies update.
		// We expect no update since one of the given policies are invalid.
		p3, _ := makePolicyItem(t, ns1)
		err = stream.Send(&servicepb.VerifierBatch{
			Update: &servicepb.VerifierUpdates{
				NamespacePolicies: &applicationpb.NamespacePolicies{
					Policies: []*applicationpb.PolicyItem{
						p3,
						policy.MakePolicy(t, ns2, policy.MakeECDSAThresholdRuleNsPolicy([]byte("bad-key"))),
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

		cp := defaultCryptoParameters(t)

		ns1Policy, ns1Signer := makePolicyItem(t, ns1)
		ns2Policy, _ := makePolicyItem(t, ns2)
		err = stream.Send(&servicepb.VerifierBatch{
			Update: &servicepb.VerifierUpdates{
				Config: cp.update.Config,
				NamespacePolicies: &applicationpb.NamespacePolicies{
					Policies: []*applicationpb.PolicyItem{ns1Policy, ns2Policy},
				},
			},
		})
		require.NoError(t, err)

		ns2PolicyUpdate, ns2Signer := makePolicyItem(t, ns2)
		err = stream.Send(&servicepb.VerifierBatch{
			Update: &servicepb.VerifierUpdates{
				NamespacePolicies: &applicationpb.NamespacePolicies{
					Policies: []*applicationpb.PolicyItem{ns2PolicyUpdate},
				},
			},
		})
		require.NoError(t, err)

		endorse(t, tx, ns1Signer, ns2Signer)
		requireTestCase(t, stream, &testCase{
			req: &servicepb.TxWithRef{
				Ref:     committerpb.NewTxRef(fakeTxID, 1, 1),
				Content: tx,
			},
			expectedStatus: committerpb.Status_COMMITTED,
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

	cp := defaultCryptoParameters(t)

	// Each policy update will update a unique namespace, and the common namespace.
	updateCount := len(ns) - 1
	uniqueNsEndorsers := make([]*sigtest.NsEndorser, updateCount)
	commonNsEndorsers := make([]*sigtest.NsEndorser, updateCount)
	for i := range updateCount {
		uniqueNsPolicy, uniqueNsEndorser := makePolicyItem(t, ns[i])
		uniqueNsEndorsers[i] = uniqueNsEndorser
		commonNsPolicy, commonNsEndorser := makePolicyItem(t, ns[len(ns)-1])
		commonNsEndorsers[i] = commonNsEndorser
		p := &servicepb.VerifierUpdates{
			Config: cp.update.Config,
			NamespacePolicies: &applicationpb.NamespacePolicies{
				Policies: []*applicationpb.PolicyItem{uniqueNsPolicy, commonNsPolicy},
			},
		}
		err = stream.Send(&servicepb.VerifierBatch{
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
		endorse(t, tx, append(uniqueNsEndorsers, commonNsEndorsers[i])...)
		require.NoError(t, stream.Send(&servicepb.VerifierBatch{
			Requests: []*servicepb.TxWithRef{{
				Ref:     committerpb.NewTxRef(fakeTxID, 0, 0),
				Content: tx,
			}},
		}))

		txStatus, err := stream.Recv()
		require.NoError(t, err)
		require.NotNil(t, txStatus)
		require.Len(t, txStatus.Status, 1)
		if txStatus.Status[0].Status == committerpb.Status_COMMITTED {
			success++
		}
	}
	require.Equal(t, 1, success)

	// The following TX updates all the namespaces but the common one.
	// It must succeed.
	tx = makeTX(ns[:updateCount]...)
	endorse(t, tx, uniqueNsEndorsers...)
	requireTestCase(t, stream, &testCase{
		req: &servicepb.TxWithRef{
			Ref:     committerpb.NewTxRef(fakeTxID, 1, 1),
			Content: tx,
		},
		expectedStatus: committerpb.Status_COMMITTED,
	})
}

type testCase struct {
	update         *servicepb.VerifierUpdates
	req            *servicepb.TxWithRef
	expectedStatus committerpb.Status
}

func endorse(t *testing.T, tx *applicationpb.Tx, signers ...*sigtest.NsEndorser) {
	t.Helper()
	tx.Endorsements = make([]*applicationpb.Endorsements, len(signers))
	for i, s := range signers {
		endorsements, err := s.EndorseTxNs(fakeTxID, tx, i)
		require.NoError(t, err)
		tx.Endorsements[i] = endorsements
	}
}

func makeTX(namespaces ...string) *applicationpb.Tx {
	tx := &applicationpb.Tx{
		Namespaces: make([]*applicationpb.TxNamespace, len(namespaces)),
	}
	for i, ns := range namespaces {
		tx.Namespaces[i] = &applicationpb.TxNamespace{
			NsId:      ns,
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{{
				Key: []byte("0001"),
			}},
		}
	}
	return tx
}

func makePolicyItem(t *testing.T, ns string) (*applicationpb.PolicyItem, *sigtest.NsEndorser) {
	t.Helper()
	signingKey, verificationKey := sigtest.NewKeyPair(signature.Ecdsa)
	txEndorser, err := sigtest.NewNsEndorserFromKey(signature.Ecdsa, signingKey)
	require.NoError(t, err)
	return policy.MakePolicy(t, ns, policy.MakeECDSAThresholdRuleNsPolicy(verificationKey)), txEndorser
}

func requireTestCase(
	t *testing.T,
	stream servicepb.Verifier_StartStreamClient,
	tt *testCase,
) {
	t.Helper()
	err := stream.Send(&servicepb.VerifierBatch{
		Update:   tt.update,
		Requests: []*servicepb.TxWithRef{tt.req},
	})
	require.NoError(t, err)

	txStatus, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, txStatus)
	require.Len(t, txStatus.Status, 1)
	resp := txStatus.Status[0]
	require.NotNil(t, resp)
	test.RequireProtoEqual(t, tt.req.Ref, resp.Ref)
	require.Equal(t, tt.expectedStatus.String(), resp.Status.String())
}

// State test state.
type State struct {
	Service *Server
	Client  servicepb.VerifierClient
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
	stream servicepb.Verifier_StartStreamClient,
	timeout time.Duration,
) ([]*committerpb.TxStatus, bool) {
	t.Helper()
	outputChan := make(chan []*committerpb.TxStatus, 1)
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	go func() {
		response, err := stream.Recv()
		if err == nil && response != nil && len(response.Status) > 0 {
			channel.NewWriter(ctx, outputChan).Write(response.Status)
		}
	}()
	return channel.NewReader(ctx, outputChan).Read()
}

func defaultCryptoParameters(t *testing.T) cryptoParameters {
	t.Helper()
	ret := cryptoParameters{
		cryptoPath: t.TempDir(),
	}

	metaTxSigningKey, metaTxVerificationKey := sigtest.NewKeyPair(signature.Ecdsa)
	configBlock, err := workload.CreateDefaultConfigBlockWithCrypto(ret.cryptoPath, &workload.ConfigBlock{
		MetaNamespaceVerificationKey: metaTxVerificationKey,
		PeerOrganizationCount:        2,
	})
	require.NoError(t, err)
	ret.metaTxEndorser, err = sigtest.NewNsEndorserFromKey(signature.Ecdsa, metaTxSigningKey)
	require.NoError(t, err)

	dataTxSigningKey, dataTxVerificationKey := sigtest.NewKeyPair(signature.Ecdsa)
	ret.dataTxEndorser, err = sigtest.NewNsEndorserFromKey(signature.Ecdsa, dataTxSigningKey)
	require.NoError(t, err)
	ret.update = &servicepb.VerifierUpdates{
		Config: &applicationpb.ConfigTransaction{
			Envelope: configBlock.Data.Data[0],
		},
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{
				policy.MakePolicy(t, "1", policy.MakeECDSAThresholdRuleNsPolicy(dataTxVerificationKey)),
			},
		},
	}
	return ret
}

func defaultConfigWithTLS(tlsConfig connection.TLSConfig) *Config {
	return &Config{
		Server: connection.NewLocalHostServer(tlsConfig),
		ParallelExecutor: ExecutorConfig{
			BatchSizeCutoff:   3,
			BatchTimeCutoff:   1 * time.Hour,
			Parallelism:       3,
			ChannelBufferSize: 1,
		},
		Monitoring: monitoring.Config{
			Server: connection.NewLocalHostServer(test.InsecureTLSConfig),
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
) servicepb.VerifierClient {
	t.Helper()
	return test.CreateClientWithTLS(t, ep, tlsCfg, servicepb.NewVerifierClient)
}
