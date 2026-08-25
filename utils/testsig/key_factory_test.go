/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig

import (
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

func TestEndToEnd(t *testing.T) {
	t.Parallel()
	for _, scheme := range signature.AllSchemes {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			priv, pub := NewKeyPair(scheme)
			v, err := signature.NewNsVerifierFromKey(scheme, pub)
			require.NoError(t, err)
			e, err := NewNsEndorserFromKey(scheme, priv)
			require.NoError(t, err)
			txID := "test"
			tx := &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       "0",
					NsVersion:  0,
					ReadWrites: make([]*applicationpb.ReadWrite, 0),
				}},
			}
			endorsement, err := e.EndorseTxNs(txID, tx, 0)
			tx.Endorsements = []*applicationpb.Endorsements{endorsement}
			require.NoError(t, err)
			require.NoError(t, v.VerifyNs(txID, tx, 0))
		})
	}
}

func TestEcdsaPem(t *testing.T) {
	t.Parallel()
	// Currently, only ECDSA is encoded to PEM, so we only test it.
	scheme := signature.Ecdsa
	dir := t.TempDir()
	pemPath := filepath.Join(dir, fmt.Sprintf("%s.pem", signature.Ecdsa))
	priv, pub := NewKeyPair(scheme)
	require.NoError(t, os.WriteFile(pemPath, append(priv, pub...), 0o600))

	v, err := signature.NewNsVerifierFromKey(scheme, pub)
	require.NoError(t, err)
	e, err := NewNsEndorserFromKey(scheme, priv)
	require.NoError(t, err)

	m, err := readPem(pemPath)
	require.NoError(t, err)

	var pemV *signature.NsVerifier
	var pemS *NsEndorser

	for key, value := range m {
		t.Log(key)
		if strings.Contains(strings.ToLower(key), "public") {
			pemV, err = signature.NewNsVerifierFromKey(scheme, value)
			require.NoError(t, err)
		}
		if strings.Contains(strings.ToLower(key), "private") {
			pemS, err = NewNsEndorserFromKey(scheme, value)
			require.NoError(t, err)
		}
	}

	require.NotNil(t, pemV, "missing public key in PEM")
	require.NotNil(t, pemS, "missing private key in PEM")

	txID := "test"
	tx := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "0",
			NsVersion:  0,
			ReadWrites: make([]*applicationpb.ReadWrite, 0),
		}},
	}

	endorsement, err := e.EndorseTxNs(txID, tx, 0)
	require.NoError(t, err)
	tx.Endorsements = []*applicationpb.Endorsements{endorsement}
	require.NoError(t, pemV.VerifyNs(txID, tx, 0))

	endorsement, err = pemS.EndorseTxNs(txID, tx, 0)
	require.NoError(t, err)
	tx.Endorsements = []*applicationpb.Endorsements{endorsement}
	require.NoError(t, v.VerifyNs(txID, tx, 0))
}

// TestEcdsaKeyPairWithSeedIsStable pins the seed -> key mapping. Fixed seeds are used across
// the load generator and its tests, so a change to either the scalar derivation or the way the
// key is built from it silently invalidates every artifact generated from a given seed. The
// expected scalars were captured from the original implementation.
func TestEcdsaKeyPairWithSeedIsStable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		seed       int64
		wantScalar string
	}{
		{
			name:       "zero seed",
			seed:       0,
			wantScalar: "48dda5bbe9171a6656206ec56c595c5834b6cf38c5fe71bcb44fe43833aee9df",
		},
		{
			name:       "one",
			seed:       1,
			wantScalar: "6c70d57af53dbf4d95253503dd5abe8c49e953236fd23851108b92bbec8ac907",
		},
		{
			name:       "arbitrary seed",
			seed:       42,
			wantScalar: "0d0960b18e45fc0f2c2242904eb4d50921f2b6fa8434bd7015904e28ba55ba81",
		},
		{
			// This scalar is only 31 bytes wide, so it must be left-padded to the curve's
			// 32-byte size. A raw big.Int encoding would be rejected as a short key.
			name:       "scalar narrower than the curve size",
			seed:       115,
			wantScalar: "0063661b93d5639df7ef18c3372f2c10a51c2a41d36cb2e2b308bb165dcad263",
		},
		{
			name:       "negative seed",
			seed:       -1,
			wantScalar: "dab9bad679ac69aab7717528842fb867663afa6d4822d159cfcedbe5b6819eb9",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			priv, pub := NewKeyPairWithSeed(signature.Ecdsa, tc.seed)

			key, err := ParseSigningKey(priv)
			require.NoError(t, err)
			rawScalar, err := key.Bytes()
			require.NoError(t, err)
			require.Equal(t, tc.wantScalar, hex.EncodeToString(rawScalar))

			// Pinning the scalar alone would not catch a change to how the public point is
			// derived from it, which would hand out a pair that cannot verify its own signatures.
			wantPub, err := SerializeVerificationKey(&key.PublicKey)
			require.NoError(t, err)
			require.Equal(t, wantPub, pub)
		})
	}
}

func readPem(certPath string) (map[string][]byte, error) {
	pemContent, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}

	ret := make(map[string][]byte)
	for {
		block, rest := pem.Decode(pemContent)
		if block == nil {
			break
		}
		pemContent = rest
		ret[block.Type] = pem.EncodeToMemory(block)
	}
	return ret, nil
}
