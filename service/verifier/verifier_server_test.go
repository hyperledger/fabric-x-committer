/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyperledger/fabric-lib-go/bccsp"
	bccsputils "github.com/hyperledger/fabric-lib-go/bccsp/utils"
	"github.com/hyperledger/fabric-protos-go-apiv2/msp"
	"github.com/hyperledger/fabric-x-common/common/policydsl"
	"github.com/hyperledger/fabric-x-common/core/config/configtest"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/certificate"
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

	err = stream.Send(&protosigverifierservice.VerifierBatch{})
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

	update, _, _ := defaultUpdate(t)
	err := stream.Send(&protosigverifierservice.VerifierBatch{Update: update})
	require.NoError(t, err)

	_, ok := readStream(t, stream, testTimeout)
	require.False(t, ok)
}

func TestMinimalInput(t *testing.T) {
	t.Parallel()
	test.FailHandler(t)
	c := newTestState(t, defaultConfigWithTLS(test.InsecureTLSConfig))

	stream, _ := c.Client.StartStream(t.Context())

	update, metaTxSigner, dataTxSigner := defaultUpdate(t)

	tx1 := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{{
				Key: []byte("0001"),
			}},
		}},
	}
	s, _ := dataTxSigner.SignNs(fakeTxID, tx1, 0)
	tx1.Endorsements = test.AppendToEndorsementSetsForThresholdRule(tx1.Endorsements, s)

	tx2 := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{{
				Key: []byte("0010"),
			}},
		}},
	}

	s, _ = dataTxSigner.SignNs(fakeTxID, tx2, 0)
	tx2.Endorsements = test.AppendToEndorsementSetsForThresholdRule(tx2.Endorsements, s)

	tx3 := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      committerpb.MetaNamespaceID,
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{{
				Key: []byte("0011"),
			}},
		}},
	}
	s, _ = metaTxSigner.SignNs(fakeTxID, tx3, 0)
	tx3.Endorsements = test.AppendToEndorsementSetsForThresholdRule(tx3.Endorsements, s)

	err := stream.Send(&protosigverifierservice.VerifierBatch{
		Update: update,
		Requests: []*protosigverifierservice.VerifierTx{
			{Ref: committerpb.TxRef(fakeTxID, 1, 1), Tx: tx1},
			{Ref: committerpb.TxRef(fakeTxID, 1, 1), Tx: tx2},
			{Ref: committerpb.TxRef(fakeTxID, 1, 1), Tx: tx3},
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

	update, _, _ := defaultUpdate(t)
	err = stream.Send(&protosigverifierservice.VerifierBatch{Update: update})
	require.NoError(t, err)

	signingIdentities := make([]*signingIdentity, 2)

	for i, org := range []string{"Org1", "Org2"} {
		signingIdentities[i] = &signingIdentity{
			CertPath: filepath.Join(configtest.GetDevConfigDir(), "crypto/"+org+"/users/User1@"+org+"/msp",
				"signcerts", "User1@"+org+"-cert.pem"),
			KeyPath: filepath.Join(configtest.GetDevConfigDir(), "crypto/"+org+"/users/User1@"+org+"/msp",
				"keystore", "priv_sk"),
			MSPID: org,
		}
	}

	serializedSigningIdentities := make([][]byte, len(signingIdentities))
	for i, si := range signingIdentities {
		serializedSigningIdentities[i] = si.serialize(t)
	}

	nsPolicy := &applicationpb.NamespacePolicy{
		Rule: &applicationpb.NamespacePolicy_MspRule{
			MspRule: protoutil.MarshalOrPanic(
				policydsl.Envelope(policydsl.And(policydsl.SignedBy(0), policydsl.SignedBy(1)),
					serializedSigningIdentities)),
		},
	}

	update = &protosigverifierservice.VerifierUpdate{
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

	data, err := signature.ASN1MarshalTxNamespace(fakeTxID, tx1.Namespaces[0])
	require.NoError(t, err)

	signatures := make([][]byte, len(signingIdentities))
	mspIDs := make([][]byte, len(signingIdentities))
	certsBytes := make([][]byte, len(signingIdentities))

	for _, certType := range []int{test.CreatorCertificate, test.CreatorID} {
		for i, si := range signingIdentities {
			signatures[i] = si.sign(t, data)
			mspIDs[i] = []byte(si.MSPID)

			switch certType {
			case test.CreatorCertificate:
				certBytes, rerr := os.ReadFile(si.CertPath)
				require.NoError(t, rerr)
				certsBytes[i] = certBytes
			case test.CreatorID:
				certID, derr := certificate.Digest(si.CertPath, bccsp.SHA256)
				require.NoError(t, derr)
				fmt.Println(string(certID))
				certsBytes[i] = certID
			}
		}

		tx1.Endorsements = []*applicationpb.Endorsements{
			test.CreateEndorsementsForSignatureRule(signatures, mspIDs, certsBytes, certType),
		}

		requireTestCase(t, stream, &testCase{
			update: update,
			req: &protosigverifierservice.VerifierTx{
				Ref: committerpb.TxRef(fakeTxID, 1, 1), Tx: tx1,
			},
			expectedStatus: applicationpb.Status_COMMITTED,
		})
	}

	// Update the config block to have SampleFabricX profile instead of
	// the default TwoOrgsSampleFabricX.
	factory := sigtest.NewSignatureFactory(signature.Ecdsa)
	_, metaTxVerificationKey := factory.NewKeys()
	configBlock, err := workload.CreateDefaultConfigBlock(&workload.ConfigBlock{
		MetaNamespaceVerificationKey: metaTxVerificationKey,
	}, configtxgen.SampleFabricX)
	require.NoError(t, err)
	update = &protosigverifierservice.VerifierUpdate{
		Config: &applicationpb.ConfigTransaction{
			Envelope: configBlock.Data.Data[0],
		},
	}

	requireTestCase(t, stream, &testCase{
		update: update,
		req: &protosigverifierservice.VerifierTx{
			Ref: committerpb.TxRef(fakeTxID, 1, 1), Tx: tx1,
		},
		expectedStatus: applicationpb.Status_ABORTED_SIGNATURE_INVALID,
	})
}

func TestBadSignature(t *testing.T) {
	t.Parallel()
	c := newTestState(t, defaultConfigQuickCutoff())

	stream, err := c.Client.StartStream(t.Context())
	require.NoError(t, err)

	update, _, _ := defaultUpdate(t)
	err = stream.Send(&protosigverifierservice.VerifierBatch{Update: update})
	require.NoError(t, err)

	requireTestCase(t, stream, &testCase{
		req: &protosigverifierservice.VerifierTx{
			Ref: committerpb.TxRef(fakeTxID, 1, 0),
			Tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*applicationpb.ReadWrite{
						{Key: make([]byte, 0)},
					},
				}},
				Endorsements: test.CreateEndorsementsForThresholdRule([]byte{0}, []byte{1}, []byte{2}),
			},
		},
		expectedStatus: applicationpb.Status_ABORTED_SIGNATURE_INVALID,
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

		update, _, _ := defaultUpdate(t)

		ns1Policy, _ := makePolicyItem(t, ns1)
		ns2Policy, _ := makePolicyItem(t, ns2)
		err = stream.Send(&protosigverifierservice.VerifierBatch{
			Update: &protosigverifierservice.VerifierUpdate{
				Config: update.Config,
				NamespacePolicies: &applicationpb.NamespacePolicies{
					Policies: []*applicationpb.PolicyItem{ns1Policy, ns2Policy},
				},
			},
		})
		require.NoError(t, err)

		// We attempt a bad policies update.
		// We expect no update since one of the given policies are invalid.
		p3, _ := makePolicyItem(t, ns1)
		err = stream.Send(&protosigverifierservice.VerifierBatch{
			Update: &protosigverifierservice.VerifierUpdate{
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

		update, _, _ := defaultUpdate(t)

		ns1Policy, ns1Signer := makePolicyItem(t, ns1)
		ns2Policy, _ := makePolicyItem(t, ns2)
		err = stream.Send(&protosigverifierservice.VerifierBatch{
			Update: &protosigverifierservice.VerifierUpdate{
				Config: update.Config,
				NamespacePolicies: &applicationpb.NamespacePolicies{
					Policies: []*applicationpb.PolicyItem{ns1Policy, ns2Policy},
				},
			},
		})
		require.NoError(t, err)

		ns2PolicyUpdate, ns2Signer := makePolicyItem(t, ns2)
		err = stream.Send(&protosigverifierservice.VerifierBatch{
			Update: &protosigverifierservice.VerifierUpdate{
				NamespacePolicies: &applicationpb.NamespacePolicies{
					Policies: []*applicationpb.PolicyItem{ns2PolicyUpdate},
				},
			},
		})
		require.NoError(t, err)

		sign(t, tx, ns1Signer, ns2Signer)
		requireTestCase(t, stream, &testCase{
			req: &protosigverifierservice.VerifierTx{
				Ref: committerpb.TxRef(fakeTxID, 1, 1),
				Tx:  tx,
			},
			expectedStatus: applicationpb.Status_COMMITTED,
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

	update, _, _ := defaultUpdate(t)

	// Each policy update will update a unique namespace, and the common namespace.
	updateCount := len(ns) - 1
	uniqueNsSigners := make([]*sigtest.NsSigner, updateCount)
	commonNsSigners := make([]*sigtest.NsSigner, updateCount)
	for i := range updateCount {
		uniqueNsPolicy, uniqueNsSigner := makePolicyItem(t, ns[i])
		uniqueNsSigners[i] = uniqueNsSigner
		commonNsPolicy, commonNsSigner := makePolicyItem(t, ns[len(ns)-1])
		commonNsSigners[i] = commonNsSigner
		p := &protosigverifierservice.VerifierUpdate{
			Config: update.Config,
			NamespacePolicies: &applicationpb.NamespacePolicies{
				Policies: []*applicationpb.PolicyItem{uniqueNsPolicy, commonNsPolicy},
			},
		}
		err = stream.Send(&protosigverifierservice.VerifierBatch{
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
		require.NoError(t, stream.Send(&protosigverifierservice.VerifierBatch{
			Requests: []*protosigverifierservice.VerifierTx{{
				Ref: committerpb.TxRef(fakeTxID, 0, 0),
				Tx:  tx,
			}},
		}))

		txStatus, err := stream.Recv()
		require.NoError(t, err)
		require.NotNil(t, txStatus)
		require.Len(t, txStatus.Responses, 1)
		if txStatus.Responses[0].Status == applicationpb.Status_COMMITTED {
			success++
		}
	}
	require.Equal(t, 1, success)

	// The following TX updates all the namespaces but the common one.
	// It must succeed.
	tx = makeTX(ns[:updateCount]...)
	sign(t, tx, uniqueNsSigners...)
	requireTestCase(t, stream, &testCase{
		req: &protosigverifierservice.VerifierTx{
			Ref: committerpb.TxRef(fakeTxID, 1, 1),
			Tx:  tx,
		},
		expectedStatus: applicationpb.Status_COMMITTED,
	})
}

type testCase struct {
	update         *protosigverifierservice.VerifierUpdate
	req            *protosigverifierservice.VerifierTx
	expectedStatus applicationpb.Status
}

func sign(t *testing.T, tx *applicationpb.Tx, signers ...*sigtest.NsSigner) {
	t.Helper()
	tx.Endorsements = make([]*applicationpb.Endorsements, len(signers))
	for i, s := range signers {
		s, err := s.SignNs(fakeTxID, tx, i)
		require.NoError(t, err)
		tx.Endorsements[i] = test.CreateEndorsementsForThresholdRule(s)[0]
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

func makePolicyItem(t *testing.T, ns string) (*applicationpb.PolicyItem, *sigtest.NsSigner) {
	t.Helper()
	factory := sigtest.NewSignatureFactory(signature.Ecdsa)
	signingKey, verificationKey := factory.NewKeys()
	txSigner, err := factory.NewSigner(signingKey)
	require.NoError(t, err)
	return policy.MakePolicy(t, ns, policy.MakeECDSAThresholdRuleNsPolicy(verificationKey)), txSigner
}

func requireTestCase(
	t *testing.T,
	stream protosigverifierservice.Verifier_StartStreamClient,
	tt *testCase,
) {
	t.Helper()
	err := stream.Send(&protosigverifierservice.VerifierBatch{
		Update:   tt.update,
		Requests: []*protosigverifierservice.VerifierTx{tt.req},
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
) ([]*protosigverifierservice.VerifierResponse, bool) {
	t.Helper()
	outputChan := make(chan []*protosigverifierservice.VerifierResponse, 1)
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

func defaultUpdate(t *testing.T) (
	update *protosigverifierservice.VerifierUpdate, metaTxSigner, dataTxSigner *sigtest.NsSigner,
) {
	t.Helper()
	factory := sigtest.NewSignatureFactory(signature.Ecdsa)
	metaTxSigningKey, metaTxVerificationKey := factory.NewKeys()
	configBlock, err := workload.CreateDefaultConfigBlock(&workload.ConfigBlock{
		MetaNamespaceVerificationKey: metaTxVerificationKey,
	}, configtxgen.TwoOrgsSampleFabricX)
	require.NoError(t, err)
	metaTxSigner, err = factory.NewSigner(metaTxSigningKey)
	require.NoError(t, err)

	dataTxSigningKey, dataTxVerificationKey := factory.NewKeys()
	dataTxSigner, err = factory.NewSigner(dataTxSigningKey)
	require.NoError(t, err)
	update = &protosigverifierservice.VerifierUpdate{
		Config: &applicationpb.ConfigTransaction{
			Envelope: configBlock.Data.Data[0],
		},
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{
				policy.MakePolicy(t, "1", policy.MakeECDSAThresholdRuleNsPolicy(dataTxVerificationKey)),
			},
		},
	}
	return update, metaTxSigner, dataTxSigner
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

// A signingIdentity represents an MSP signing identity.
type signingIdentity struct {
	CertPath string
	KeyPath  string
	MSPID    string
}

// serialize returns the probobuf encoding of an msp.SerializedIdenity.
func (s *signingIdentity) serialize(t *testing.T) []byte {
	t.Helper()
	cert, err := os.ReadFile(s.CertPath)
	require.NoError(t, err)
	si, err := proto.Marshal(&msp.SerializedIdentity{
		Mspid:   s.MSPID,
		IdBytes: cert,
	})
	require.NoError(t, err)
	return si
}

// sign computes a SHA256 message digest if key is ECDSA,
// signs it with the associated private key, and returns the
// signature. Low-S normlization is applied for ECDSA signatures.
func (s *signingIdentity) sign(t *testing.T, msg []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(msg)
	pemKey, err := os.ReadFile(s.KeyPath)
	require.NoError(t, err)

	block, _ := pem.Decode(pemKey)
	if block.Type != "EC PRIVATE KEY" && block.Type != "PRIVATE KEY" {
		t.Fatalf("file %s does not contain a private key", s.KeyPath)
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)

	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		r, _s, err := ecdsa.Sign(rand.Reader, k, digest[:])
		require.NoError(t, err)
		sig, err := bccsputils.MarshalECDSASignature(r, _s)
		require.NoError(t, err)
		s, err := bccsputils.SignatureToLowS(&k.PublicKey, sig)
		require.NoError(t, err)
		return s
	case ed25519.PrivateKey:
		return ed25519.Sign(k, msg)
	default:
		t.Fatalf("unexpected key type: %T", key)
	}

	return nil
}
