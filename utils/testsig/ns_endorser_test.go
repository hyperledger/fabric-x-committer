/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig

import (
	"encoding/hex"
	"fmt"
	"strings"
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

	priv, _ := NewKeyPair(signature.Ecdsa)
	for _, tc := range []struct {
		name          string
		scheme        string
		key           []byte
		expectedError string
		nilEndorser   bool
	}{
		{
			name:        "NoScheme returns nil endorser",
			key:         []byte("key"),
			scheme:      signature.NoScheme,
			nilEndorser: true,
		},
		{
			name:        "empty scheme returns nil endorser",
			key:         []byte("key"),
			scheme:      "",
			nilEndorser: true,
		},
		{
			name:          "unsupported scheme returns error",
			key:           []byte("key"),
			scheme:        "UNSUPPORTED",
			expectedError: "not supported",
		},
		{
			name:   "case insensitive scheme",
			key:    priv,
			scheme: strings.ToLower(signature.Ecdsa),
		},
		{
			name:          "invalid ECDSA key returns error",
			scheme:        signature.Ecdsa,
			expectedError: "cannot decode PEM block content",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			endorser, err := NewNsEndorserFromKey(tc.scheme, tc.key)
			if tc.expectedError != "" {
				require.ErrorContains(t, err, tc.expectedError)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, endorser)
			if tc.nilEndorser {
				require.Nil(t, endorser.endorser)
			}
		})
	}
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

	for _, tc := range []struct {
		name      string
		certType  int
		peerCount uint32
	}{
		{
			name:      "single MSP identity",
			certType:  test.CreatorCertificate,
			peerCount: 1,
		},
		{
			name:      "3 MSP identity",
			certType:  test.CreatorCertificate,
			peerCount: 3,
		},
		{
			name:      "single ID",
			certType:  test.CreatorID,
			peerCount: 1,
		},
		{
			name:      "3 IDs",
			certType:  test.CreatorID,
			peerCount: 3,
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

			endorser, err := NewNsEndorserFromMsp(tc.certType, identities...)
			require.NoError(t, err)

			endorsement, err := endorser.EndorseTxNs(testTxID, &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       "ns0",
					NsVersion:  1,
					ReadWrites: []*applicationpb.ReadWrite{},
				}},
			}, 0)
			require.NoError(t, err)
			require.NotNil(t, endorsement)
			require.Len(t, endorsement.EndorsementsWithIdentity, int(tc.peerCount))

			for i := range tc.peerCount {
				require.NotEmpty(t, endorsement.EndorsementsWithIdentity[i].Endorsement)
				require.NotNil(t, endorsement.EndorsementsWithIdentity[i].Identity)

				if tc.certType == test.CreatorID {
					identityStr := endorsement.EndorsementsWithIdentity[i].Identity.String()
					require.Contains(t, identityStr, "certificate_id:")
					require.Contains(t, identityStr, "\"")
				}
			}
		})
	}
}

func TestEndorseTxNsEdgeCases(t *testing.T) {
	t.Parallel()

	tx := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "ns0",
			NsVersion:  1,
			ReadWrites: []*applicationpb.ReadWrite{},
		}},
	}

	t.Run("nil endorser returns dummy endorsement", func(t *testing.T) {
		t.Parallel()
		endorser, err := NewNsEndorserFromKey(signature.NoScheme, nil)
		require.NoError(t, err)

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
		_, err = endorser.EndorseTxNs("test-tx", tx, -1)
		require.ErrorContains(t, err, "out of range")
	})

	t.Run("namespace index out of range returns error", func(t *testing.T) {
		t.Parallel()
		priv, _ := NewKeyPair(signature.Ecdsa)
		endorser, err := NewNsEndorserFromKey(signature.Ecdsa, priv)
		require.NoError(t, err)
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

		twoNsTX := &applicationpb.Tx{
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

		endorsement0, err := endorser.EndorseTxNs(testTxID, twoNsTX, 0)
		require.NoError(t, err)

		endorsement1, err := endorser.EndorseTxNs(testTxID, twoNsTX, 1)
		require.NoError(t, err)

		twoNsTX.Endorsements = []*applicationpb.Endorsements{endorsement0, endorsement1}

		err = verifier.VerifyNs(testTxID, twoNsTX, 0)
		require.NoError(t, err)

		err = verifier.VerifyNs(testTxID, twoNsTX, 1)
		require.NoError(t, err)
	})

	t.Run("different txIDs produce different endorsements", func(t *testing.T) {
		t.Parallel()
		priv, _ := NewKeyPair(signature.Ecdsa)
		endorser, err := NewNsEndorserFromKey(signature.Ecdsa, priv)
		require.NoError(t, err)

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

	sigs := [][]byte{[]byte("signature1"), []byte("signature2"), []byte("signature3")}
	for sigCount := range 4 {
		t.Run(fmt.Sprintf("%d signatures", sigCount), func(t *testing.T) {
			t.Parallel()
			sets := CreateEndorsementsForThresholdRule(sigs[:sigCount]...)
			require.Len(t, sets, sigCount)
			for i := range sigCount {
				require.Equal(t, sigs[i], sets[i].EndorsementsWithIdentity[0].Endorsement)
			}
		})
	}
}

func TestCreateEndorsementsForSignatureRule(t *testing.T) {
	t.Parallel()

	signatures := [][]byte{[]byte("sig1"), []byte("sig2"), []byte("sig3")}
	mspIDs := [][]byte{[]byte("msp1"), []byte("msp2"), []byte("msp3")}
	certs := [][]byte{[]byte("cert1"), []byte("cert2"), []byte("cert3")}

	for _, tc := range []struct {
		name     string
		certType int
		sigCount int
	}{
		{
			name:     "3 certificates",
			certType: test.CreatorCertificate,
			sigCount: 3,
		},
		{
			name:     "3 IDs",
			certType: test.CreatorID,
			sigCount: 3,
		},
		{
			name:     "single certificate",
			certType: test.CreatorCertificate,
			sigCount: 1,
		},
		{
			name:     "single ID",
			certType: test.CreatorID,
			sigCount: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			set := CreateEndorsementsForSignatureRule(
				signatures[:tc.sigCount], mspIDs[:tc.sigCount], certs[:tc.sigCount], tc.certType,
			)
			require.NotNil(t, set)
			require.Len(t, set.EndorsementsWithIdentity, tc.sigCount)

			for i := range tc.sigCount {
				require.Equal(t, signatures[i], set.EndorsementsWithIdentity[i].Endorsement)
				require.Equal(t, string(mspIDs[i]), set.EndorsementsWithIdentity[i].Identity.MspId)
				require.NotNil(t, set.EndorsementsWithIdentity[i].Identity)

				if tc.certType == test.CreatorID {
					expectedID := hex.EncodeToString(certs[i])
					require.Contains(t, set.EndorsementsWithIdentity[i].Identity.String(), expectedID)
				}
			}
		})
	}

	t.Run("empty inputs", func(t *testing.T) {
		t.Parallel()
		set := CreateEndorsementsForSignatureRule(nil, nil, nil, test.CreatorCertificate)

		require.NotNil(t, set)
		require.Empty(t, set.EndorsementsWithIdentity)
	})
}
