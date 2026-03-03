/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig

import (
	"encoding/hex"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

const testTxID = "test-tx-id"

func TestNewNsEndorserFromKey(t *testing.T) {
	t.Parallel()

	for _, scheme := range signature.AllRealSchemes {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			priv, _ := NewKeyPair(scheme)

			endorser, err := NewNsEndorserFromKey(scheme, priv)
			require.NoError(t, err)
			require.NotNil(t, endorser)
			require.NotNil(t, endorser.endorser)
		})
	}

	t.Run("NoScheme returns nil endorser", func(t *testing.T) {
		t.Parallel()
		endorser, err := NewNsEndorserFromKey(signature.NoScheme, nil)
		require.NoError(t, err)
		require.NotNil(t, endorser)
		require.Nil(t, endorser.endorser)
	})

	t.Run("empty scheme returns nil endorser", func(t *testing.T) {
		t.Parallel()
		endorser, err := NewNsEndorserFromKey("", nil)
		require.NoError(t, err)
		require.NotNil(t, endorser)
		require.Nil(t, endorser.endorser)
	})

	t.Run("unsupported scheme returns error", func(t *testing.T) {
		t.Parallel()
		_, err := NewNsEndorserFromKey("UNSUPPORTED", []byte("key"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "not supported")
	})

	t.Run("invalid ECDSA key returns error", func(t *testing.T) {
		t.Parallel()
		_, err := NewNsEndorserFromKey(signature.Ecdsa, []byte("invalid-key"))
		require.Error(t, err)
	})

	t.Run("case insensitive scheme", func(t *testing.T) {
		t.Parallel()
		priv, _ := NewKeyPair(signature.Ecdsa)
		endorser, err := NewNsEndorserFromKey("ecdsa", priv)
		require.NoError(t, err)
		require.NotNil(t, endorser)
	})
}

func TestNewNsEndorserFromMsp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		identityType int
		peerCount    uint32
	}{
		{
			name:         "single certificate",
			identityType: test.CreatorCertificate,
			peerCount:    1,
		},
		{
			name:         "single ID",
			identityType: test.CreatorID,
			peerCount:    1,
		},

		{
			name:         "3 certificates",
			identityType: test.CreatorCertificate,
			peerCount:    3,
		},
		{
			name:         "3 IDs",
			identityType: test.CreatorID,
			peerCount:    3,
		},
		{
			name:         "no identities",
			identityType: test.CreatorID,
			peerCount:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cryptoPath := t.TempDir()
			_, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoPath, &testcrypto.ConfigBlock{
				ChannelID:             "test-channel",
				PeerOrganizationCount: tc.peerCount,
			})
			require.NoError(t, err)

			identities, err := testcrypto.GetPeersIdentities(cryptoPath)
			require.NoError(t, err)
			require.Len(t, identities, int(tc.peerCount))

			endorser, err := NewNsEndorserFromMsp(tc.identityType, identities...)
			require.NoError(t, err)
			require.NotNil(t, endorser)
			require.NotNil(t, endorser.endorser)
		})
	}
}

func TestEndorseTxNsWithKeySchemes(t *testing.T) {
	t.Parallel()

	for _, scheme := range signature.AllRealSchemes {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			priv, pub := NewKeyPair(scheme)

			endorser, err := NewNsEndorserFromKey(scheme, priv)
			require.NoError(t, err)

			verifier, err := signature.NewNsVerifierFromKey(scheme, pub)
			require.NoError(t, err)

			tx := &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       "ns0",
					NsVersion:  1,
					ReadWrites: []*applicationpb.ReadWrite{},
				}},
			}

			endorsement, err := endorser.EndorseTxNs(testTxID, tx, 0)
			require.NoError(t, err)
			require.NotNil(t, endorsement)

			tx.Endorsements = []*applicationpb.Endorsements{endorsement}
			err = verifier.VerifyNs(testTxID, tx, 0)
			require.NoError(t, err)
		})
	}
}

func TestEndorseTxNsWithMSP(t *testing.T) {
	t.Parallel()

	t.Run("single MSP identity", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()
		_, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoPath, &testcrypto.ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 1,
		})
		require.NoError(t, err)

		identities, err := testcrypto.GetPeersIdentities(cryptoPath)
		require.NoError(t, err)

		endorser, err := NewNsEndorserFromMsp(test.CreatorCertificate, identities...)
		require.NoError(t, err)

		tx := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:       "ns0",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{},
			}},
		}

		endorsement, err := endorser.EndorseTxNs(testTxID, tx, 0)
		require.NoError(t, err)
		require.NotNil(t, endorsement)
		require.Len(t, endorsement.EndorsementsWithIdentity, 1)
	})

	t.Run("multiple MSP identities", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()
		_, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoPath, &testcrypto.ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 3,
		})
		require.NoError(t, err)

		identities, err := testcrypto.GetPeersIdentities(cryptoPath)
		require.NoError(t, err)
		require.Len(t, identities, 3)

		endorser, err := NewNsEndorserFromMsp(test.CreatorCertificate, identities...)
		require.NoError(t, err)

		tx := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:       "ns0",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{},
			}},
		}

		endorsement, err := endorser.EndorseTxNs(testTxID, tx, 0)
		require.NoError(t, err)
		require.NotNil(t, endorsement)
		require.Len(t, endorsement.EndorsementsWithIdentity, 3)

		for i := range 3 {
			require.NotEmpty(t, endorsement.EndorsementsWithIdentity[i].Endorsement)
			require.NotNil(t, endorsement.EndorsementsWithIdentity[i].Identity)
		}
	})

	t.Run("MSP with ID type", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()
		_, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoPath, &testcrypto.ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 1,
		})
		require.NoError(t, err)

		identities, err := testcrypto.GetPeersIdentities(cryptoPath)
		require.NoError(t, err)
		require.Len(t, identities, 1)

		endorser, err := NewNsEndorserFromMsp(test.CreatorID, identities...)
		require.NoError(t, err)

		tx := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:       "ns0",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{},
			}},
		}

		endorsement, err := endorser.EndorseTxNs(testTxID, tx, 0)
		require.NoError(t, err)
		require.NotNil(t, endorsement)
		require.Len(t, endorsement.EndorsementsWithIdentity, 1)

		require.Equal(t, identities[0].GetMSPIdentifier(), endorsement.EndorsementsWithIdentity[0].Identity.MspId)
		identityStr := endorsement.EndorsementsWithIdentity[0].Identity.String()
		require.Contains(t, identityStr, "certificate_id:")
		require.Contains(t, identityStr, "\"")
	})
}

func TestEndorseTxNsEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("nil endorser returns dummy endorsement", func(t *testing.T) {
		t.Parallel()
		endorser, err := NewNsEndorserFromKey(signature.NoScheme, nil)
		require.NoError(t, err)

		tx := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:       "ns0",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{},
			}},
		}

		endorsement, err := endorser.EndorseTxNs(testTxID, tx, 0)
		require.NoError(t, err)
		require.NotNil(t, endorsement)
		require.Equal(t, dummyEndorsement, endorsement)
	})

	t.Run("negative namespace index returns error", func(t *testing.T) {
		t.Parallel()
		priv, _ := NewKeyPair(signature.Ecdsa)
		endorser, err := NewNsEndorserFromKey(signature.Ecdsa, priv)
		require.NoError(t, err)

		tx := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:       "ns0",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{},
			}},
		}

		_, err = endorser.EndorseTxNs("test-tx", tx, -1)
		require.ErrorContains(t, err, "out of range")
	})

	t.Run("namespace index out of range returns error", func(t *testing.T) {
		t.Parallel()
		priv, _ := NewKeyPair(signature.Ecdsa)
		endorser, err := NewNsEndorserFromKey(signature.Ecdsa, priv)
		require.NoError(t, err)

		tx := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:       "ns0",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{},
			}},
		}

		_, err = endorser.EndorseTxNs("test-tx", tx, 5)
		require.ErrorContains(t, err, "out of range")
	})

	t.Run("multiple namespaces", func(t *testing.T) {
		t.Parallel()
		priv, pub := NewKeyPair(signature.Ecdsa)

		endorser, err := NewNsEndorserFromKey(signature.Ecdsa, priv)
		require.NoError(t, err)

		verifier, err := signature.NewNsVerifierFromKey(signature.Ecdsa, pub)
		require.NoError(t, err)

		tx := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{
				{
					NsId:       "ns0",
					NsVersion:  1,
					ReadWrites: []*applicationpb.ReadWrite{},
				},
				{
					NsId:       "ns1",
					NsVersion:  2,
					ReadWrites: []*applicationpb.ReadWrite{},
				},
			},
		}

		endorsement0, err := endorser.EndorseTxNs(testTxID, tx, 0)
		require.NoError(t, err)

		endorsement1, err := endorser.EndorseTxNs(testTxID, tx, 1)
		require.NoError(t, err)

		tx.Endorsements = []*applicationpb.Endorsements{endorsement0, endorsement1}

		err = verifier.VerifyNs(testTxID, tx, 0)
		require.NoError(t, err)

		err = verifier.VerifyNs(testTxID, tx, 1)
		require.NoError(t, err)
	})

	t.Run("different txIDs produce different endorsements", func(t *testing.T) {
		t.Parallel()
		priv, _ := NewKeyPair(signature.Ecdsa)
		endorser, err := NewNsEndorserFromKey(signature.Ecdsa, priv)
		require.NoError(t, err)

		tx := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:       "ns0",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{},
			}},
		}

		endorsement1, err := endorser.EndorseTxNs("tx-id-1", tx, 0)
		require.NoError(t, err)

		endorsement2, err := endorser.EndorseTxNs("tx-id-2", tx, 0)
		require.NoError(t, err)

		require.NotEqual(t,
			endorsement1.EndorsementsWithIdentity[0].Endorsement,
			endorsement2.EndorsementsWithIdentity[0].Endorsement)
	})
}

func TestCreateEndorsementsForThresholdRule(t *testing.T) {
	t.Parallel()

	t.Run("single signature", func(t *testing.T) {
		t.Parallel()
		sig := []byte("signature1")
		sets := CreateEndorsementsForThresholdRule(sig)

		require.Len(t, sets, 1)
		require.Len(t, sets[0].EndorsementsWithIdentity, 1)
		require.Equal(t, sig, sets[0].EndorsementsWithIdentity[0].Endorsement)
	})

	t.Run("multiple signatures", func(t *testing.T) {
		t.Parallel()
		sig1 := []byte("signature1")
		sig2 := []byte("signature2")
		sig3 := []byte("signature3")

		sets := CreateEndorsementsForThresholdRule(sig1, sig2, sig3)

		require.Len(t, sets, 3)
		require.Equal(t, sig1, sets[0].EndorsementsWithIdentity[0].Endorsement)
		require.Equal(t, sig2, sets[1].EndorsementsWithIdentity[0].Endorsement)
		require.Equal(t, sig3, sets[2].EndorsementsWithIdentity[0].Endorsement)
	})

	t.Run("empty signatures", func(t *testing.T) {
		t.Parallel()
		sets := CreateEndorsementsForThresholdRule()
		require.Empty(t, sets)
	})
}

func TestCreateEndorsementsForSignatureRule(t *testing.T) {
	t.Parallel()

	t.Run("with certificate type", func(t *testing.T) {
		t.Parallel()
		signatures := [][]byte{[]byte("sig1"), []byte("sig2")}
		mspIDs := [][]byte{[]byte("msp1"), []byte("msp2")}
		certs := [][]byte{[]byte("cert1"), []byte("cert2")}

		set := CreateEndorsementsForSignatureRule(signatures, mspIDs, certs, test.CreatorCertificate)

		require.NotNil(t, set)
		require.Len(t, set.EndorsementsWithIdentity, 2)

		require.Equal(t, signatures[0], set.EndorsementsWithIdentity[0].Endorsement)
		require.Equal(t, signatures[1], set.EndorsementsWithIdentity[1].Endorsement)

		require.Equal(t, "msp1", set.EndorsementsWithIdentity[0].Identity.MspId)
		require.Equal(t, "msp2", set.EndorsementsWithIdentity[1].Identity.MspId)

		require.NotNil(t, set.EndorsementsWithIdentity[0].Identity)
		require.NotNil(t, set.EndorsementsWithIdentity[1].Identity)
	})

	t.Run("with ID type", func(t *testing.T) {
		t.Parallel()
		signatures := [][]byte{[]byte("sig1")}
		mspIDs := [][]byte{[]byte("msp1")}
		certIDs := [][]byte{{0x01, 0x02, 0x03}}

		set := CreateEndorsementsForSignatureRule(signatures, mspIDs, certIDs, test.CreatorID)

		require.NotNil(t, set)
		require.Len(t, set.EndorsementsWithIdentity, 1)

		require.Equal(t, signatures[0], set.EndorsementsWithIdentity[0].Endorsement)
		require.Equal(t, "msp1", set.EndorsementsWithIdentity[0].Identity.MspId)

		expectedID := hex.EncodeToString(certIDs[0])
		require.Contains(t, set.EndorsementsWithIdentity[0].Identity.String(), expectedID)
	})

	t.Run("empty inputs", func(t *testing.T) {
		t.Parallel()
		set := CreateEndorsementsForSignatureRule(nil, nil, nil, test.CreatorCertificate)

		require.NotNil(t, set)
		require.Empty(t, set.EndorsementsWithIdentity)
	})

	t.Run("single signature", func(t *testing.T) {
		t.Parallel()
		signatures := [][]byte{[]byte("single-sig")}
		mspIDs := [][]byte{[]byte("single-msp")}
		certs := [][]byte{[]byte("single-cert")}

		set := CreateEndorsementsForSignatureRule(signatures, mspIDs, certs, test.CreatorCertificate)

		require.NotNil(t, set)
		require.Len(t, set.EndorsementsWithIdentity, 1)
		require.Equal(t, signatures[0], set.EndorsementsWithIdentity[0].Endorsement)
	})
}
