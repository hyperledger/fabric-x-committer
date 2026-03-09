/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

func TestNewNsVerifierFromKey(t *testing.T) {
	t.Parallel()

	for _, scheme := range append(signature.AllSchemes, "") {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			_, pub := testsig.NewKeyPair(scheme)
			verifier, err := signature.NewNsVerifierFromKey(scheme, pub)
			require.NoError(t, err)
			require.NotNil(t, verifier)
		})
	}

	t.Run("unsupported scheme returns error", func(t *testing.T) {
		t.Parallel()
		_, err := signature.NewNsVerifierFromKey("UNKNOWN", []byte("key"))
		require.ErrorContains(t, err, "scheme 'UNKNOWN' not supported")
	})

	t.Run("case insensitive scheme", func(t *testing.T) {
		t.Parallel()
		_, pub := testsig.NewKeyPair(signature.Eddsa)
		verifier, err := signature.NewNsVerifierFromKey(strings.ToLower(signature.Eddsa), pub)
		require.NoError(t, err)
		require.NotNil(t, verifier)
	})
}

func TestEcdsaVerifierErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		pemData     []byte
		expectedErr string
	}{
		{
			name:        "invalid PEM block",
			pemData:     []byte("not a pem"),
			expectedErr: "failed to decode PEM block",
		},
		{
			name: "wrong PEM block type",
			pemData: pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: []byte("data"),
			}),
			expectedErr: "failed to decode PEM block",
		},
		{
			name: "invalid public key data",
			pemData: pem.EncodeToMemory(&pem.Block{
				Type:  "PUBLIC KEY",
				Bytes: []byte("invalid data"),
			}),
			expectedErr: "cannot parse public key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := signature.NewNsVerifierFromKey(signature.Ecdsa, tc.pemData)
			require.ErrorContains(t, err, tc.expectedErr)
		})
	}

	t.Run("non-ECDSA public key", func(t *testing.T) {
		t.Parallel()
		pubKey, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)

		pubKeyBytes, err := x509.MarshalPKIXPublicKey(pubKey)
		require.NoError(t, err)

		pemBytes := pem.EncodeToMemory(&pem.Block{
			Type:  "PUBLIC KEY",
			Bytes: pubKeyBytes,
		})

		_, err = signature.NewNsVerifierFromKey(signature.Ecdsa, pemBytes)
		require.ErrorContains(t, err, "failed to assert public key type to ECDSA")
	})
}

func TestBlsVerifierErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		keyBytes    []byte
		expectedErr string
	}{
		{
			name:        "invalid key bytes",
			keyBytes:    []byte("invalid"),
			expectedErr: "cannot set G2 from verification key bytes",
		},
		{
			name:        "empty key bytes",
			keyBytes:    []byte{},
			expectedErr: "cannot set G2 from verification key bytes",
		},
		{
			name:        "wrong length key bytes",
			keyBytes:    make([]byte, 10),
			expectedErr: "cannot set G2 from verification key bytes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := signature.NewNsVerifierFromKey(signature.Bls, tc.keyBytes)
			require.ErrorContains(t, err, tc.expectedErr)
		})
	}
}

func TestVerifyNsErrors(t *testing.T) {
	t.Parallel()

	verifier, verifierErr := signature.NewNsVerifierFromKey(signature.NoScheme, nil)
	require.NoError(t, verifierErr)

	for _, tc := range []struct {
		name         string
		nsCount      int
		endorseCount int
		nsIndex      int
		expectedErr  string
	}{
		{
			name:         "negative namespace index",
			nsCount:      1,
			endorseCount: 1,
			nsIndex:      -1,
			expectedErr:  "namespace index out of range",
		},
		{
			name:         "namespace index exceeds namespaces length",
			nsCount:      2,
			endorseCount: 3,
			nsIndex:      2,
			expectedErr:  "namespace index out of range",
		},
		{
			name:         "namespace index exceeds endorsements length",
			nsCount:      3,
			endorseCount: 2,
			nsIndex:      2,
			expectedErr:  "namespace index out of range",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := makeTx(tc.nsCount, tc.endorseCount)
			err := verifier.VerifyNs("txid", tx, tc.nsIndex)
			require.ErrorContains(t, err, tc.expectedErr)
		})
	}

	t.Run("NONE scheme skips verification", func(t *testing.T) {
		t.Parallel()
		err := verifier.VerifyNs("txid", makeTx(1, 1), 0)
		require.NoError(t, err)
	})
}

func TestKeyVerifierNoEndorsements(t *testing.T) {
	t.Parallel()

	for _, scheme := range signature.AllRealSchemes {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			_, pub := testsig.NewKeyPair(scheme)
			verifier, err := signature.NewNsVerifierFromKey(scheme, pub)
			require.NoError(t, err)

			tx := &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       "ns",
					NsVersion:  1,
					ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k"), Value: []byte("v")}},
				}},
				Endorsements: []*applicationpb.Endorsements{{
					EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{},
				}},
			}

			err = verifier.VerifyNs("txid", tx, 0)
			require.ErrorContains(t, err, "no endorsements provided")
		})
	}
}

func TestSignatureVerification(t *testing.T) {
	t.Parallel()

	for _, scheme := range signature.AllRealSchemes {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()

			priv, pub := testsig.NewKeyPair(scheme)
			endorser, err := testsig.NewNsEndorserFromKey(scheme, priv)
			require.NoError(t, err)

			verifier, err := signature.NewNsVerifierFromKey(scheme, pub)
			require.NoError(t, err)

			tx := &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       "ns",
					NsVersion:  1,
					ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k"), Value: []byte("v")}},
				}},
			}

			t.Run("valid signature", func(t *testing.T) {
				t.Parallel()
				endorsement, err := endorser.EndorseTxNs("txid", tx, 0)
				require.NoError(t, err)

				tx.Endorsements = []*applicationpb.Endorsements{endorsement}
				err = verifier.VerifyNs("txid", tx, 0)
				require.NoError(t, err)
			})

			t.Run("invalid signature", func(t *testing.T) {
				t.Parallel()
				// Generate a signature from a different key pair
				wrongPriv, _ := testsig.NewKeyPair(scheme)
				wrongEndorser, err := testsig.NewNsEndorserFromKey(scheme, wrongPriv)
				require.NoError(t, err)

				wrongEndorsement, err := wrongEndorser.EndorseTxNs("txid", tx, 0)
				require.NoError(t, err)

				txBad := &applicationpb.Tx{
					Namespaces:   tx.Namespaces,
					Endorsements: []*applicationpb.Endorsements{wrongEndorsement},
				}
				err = verifier.VerifyNs("txid", txBad, 0)
				require.Error(t, err)
				require.ErrorIs(t, err, signature.ErrSignatureMismatch)
			})
		})
	}
}

func TestBlsSignatureVerificationErrors(t *testing.T) {
	t.Parallel()

	_, pub := testsig.NewKeyPair(signature.Bls)
	verifier, err := signature.NewNsVerifierFromKey(signature.Bls, pub)
	require.NoError(t, err)

	tx := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "ns",
			NsVersion:  1,
			ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k"), Value: []byte("v")}},
		}},
	}

	t.Run("invalid signature bytes", func(t *testing.T) {
		t.Parallel()
		txBad := &applicationpb.Tx{
			Namespaces: tx.Namespaces,
			Endorsements: []*applicationpb.Endorsements{{
				EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{{
					Endorsement: []byte("bad"),
				}},
			}},
		}
		err = verifier.VerifyNs("txid", txBad, 0)
		require.ErrorContains(t, err, "cannot set G1 from signature bytes")
	})

	t.Run("wrong signature", func(t *testing.T) {
		t.Parallel()
		_, _, g1, _ := bn254.Generators()
		wrongSig := g1.Bytes()
		txBad := &applicationpb.Tx{
			Namespaces: tx.Namespaces,
			Endorsements: []*applicationpb.Endorsements{{
				EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{{
					Endorsement: wrongSig[:],
				}},
			}},
		}
		err = verifier.VerifyNs("txid", txBad, 0)
		require.ErrorIs(t, err, signature.ErrSignatureMismatch)
	})
}

func TestNewNsVerifierFromMspErrors(t *testing.T) {
	t.Parallel()

	_, err := signature.NewNsVerifierFromMsp([]byte("invalid"), nil)
	require.ErrorContains(t, err, "error updating msp verifier")
}

func TestNewNsVerifierErrors(t *testing.T) {
	t.Parallel()

	policy := &applicationpb.NamespacePolicy{}
	_, err := signature.NewNsVerifier(policy, nil)
	require.ErrorContains(t, err, "policy rule")
	require.ErrorContains(t, err, "not supported")
}

func TestRSAKeyRejection(t *testing.T) {
	t.Parallel()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	pubKeyBytes, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	require.NoError(t, err)

	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubKeyBytes,
	})

	_, err = signature.NewNsVerifierFromKey(signature.Ecdsa, pemBytes)
	require.ErrorContains(t, err, "failed to assert public key type to ECDSA")
}

func TestECDSACurveVariations(t *testing.T) {
	t.Parallel()

	for _, curve := range []elliptic.Curve{
		elliptic.P224(),
		elliptic.P256(),
		elliptic.P384(),
		elliptic.P521(),
	} {
		t.Run(curve.Params().Name, func(t *testing.T) {
			t.Parallel()

			privateKey, err := ecdsa.GenerateKey(curve, rand.Reader)
			require.NoError(t, err)

			pubKeyBytes, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
			require.NoError(t, err)

			pubKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyBytes})

			verifier, err := signature.NewNsVerifierFromKey(signature.Ecdsa, pubKeyPEM)
			require.NoError(t, err)
			require.NotNil(t, verifier)
		})
	}
}

func TestBLSValidG2PointCreation(t *testing.T) {
	t.Parallel()

	_, pub := testsig.NewKeyPair(signature.Bls)

	verifier, err := signature.NewNsVerifierFromKey(signature.Bls, pub)
	require.NoError(t, err)
	require.NotNil(t, verifier)
}

func TestMultipleNamespaceVerification(t *testing.T) {
	t.Parallel()

	priv, pub := testsig.NewKeyPair(signature.Ecdsa)
	endorser, err := testsig.NewNsEndorserFromKey(signature.Ecdsa, priv)
	require.NoError(t, err)

	verifier, err := signature.NewNsVerifierFromKey(signature.Ecdsa, pub)
	require.NoError(t, err)

	tx := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{
			{
				NsId:       "ns0",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k0"), Value: []byte("v0")}},
			},
			{
				NsId:       "ns1",
				NsVersion:  2,
				ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k1"), Value: []byte("v1")}},
			},
		},
	}

	endorsement0, err := endorser.EndorseTxNs("txid", tx, 0)
	require.NoError(t, err)

	endorsement1, err := endorser.EndorseTxNs("txid", tx, 1)
	require.NoError(t, err)

	tx.Endorsements = []*applicationpb.Endorsements{endorsement0, endorsement1}

	err = verifier.VerifyNs("txid", tx, 0)
	require.NoError(t, err)

	err = verifier.VerifyNs("txid", tx, 1)
	require.NoError(t, err)
}

func makeTx(nsCount, endorsementCount int) *applicationpb.Tx {
	tx := &applicationpb.Tx{
		Namespaces:   make([]*applicationpb.TxNamespace, nsCount),
		Endorsements: make([]*applicationpb.Endorsements, endorsementCount),
	}
	for i := range nsCount {
		tx.Namespaces[i] = &applicationpb.TxNamespace{
			NsId:       "ns",
			NsVersion:  1,
			ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k"), Value: []byte("v")}},
		}
	}
	for i := range endorsementCount {
		tx.Endorsements[i] = &applicationpb.Endorsements{
			EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{},
		}
	}
	return tx
}
