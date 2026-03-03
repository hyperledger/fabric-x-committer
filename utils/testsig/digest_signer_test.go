/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"testing"

	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

type digestSigner interface {
	Sign(digest signature.Digest) (signature.Signature, error)
}

func TestDigestSigners(t *testing.T) {
	t.Parallel()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	curEddsaSigner := &eddsaSigner{PrivateKey: privateKey}

	sk := big.NewInt(12345)
	curBlsSigner := &blsSigner{sk: sk}

	priv, _ := NewKeyPair(signature.Ecdsa)
	signingKey, err := ParseSigningKey(priv)
	require.NoError(t, err)
	curEcdsaSigner := &ecdsaSigner{signingKey: signingKey}

	for _, tc := range []struct {
		name   string
		signer digestSigner
		expLen int
	}{
		{name: signature.Eddsa, signer: curEddsaSigner, expLen: 64},
		{name: signature.Bls, signer: curBlsSigner, expLen: bn254.SizeOfG1AffineCompressed},
		{name: signature.Ecdsa, signer: curEcdsaSigner, expLen: 0}, // Variable length
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Run("successful signing", func(t *testing.T) {
				t.Parallel()
				digest := sha256.Sum256([]byte("test message"))
				sig, err := tc.signer.Sign(digest[:])
				require.NoError(t, err)
				require.NotNil(t, sig)
				require.NotEmpty(t, sig)

				if tc.expLen > 0 {
					require.Len(t, sig, tc.expLen)
				}
			})
			t.Run("different messages produce different signatures", func(t *testing.T) {
				t.Parallel()
				digest1 := sha256.Sum256([]byte("message 1"))
				sig1, err := tc.signer.Sign(digest1[:])
				require.NoError(t, err)

				digest2 := sha256.Sum256([]byte("message 2"))
				sig2, err := tc.signer.Sign(digest2[:])
				require.NoError(t, err)

				require.NotEqual(t, sig1, sig2)
			})
		})
	}
}
