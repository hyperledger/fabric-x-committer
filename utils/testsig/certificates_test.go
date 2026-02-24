/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/
package testsig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerificationAndSigningKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		curve elliptic.Curve
	}{
		{
			name:  "P256",
			curve: elliptic.P256(),
		},
		{
			name:  "P384",
			curve: elliptic.P384(),
		},
		{
			name:  "P224",
			curve: elliptic.P224(),
		},
		{
			name:  "P521",
			curve: elliptic.P521(),
		},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("Serialize %s VerificationKey", tt.name),
			func(t *testing.T) {
				t.Parallel()

				privKey, err := ecdsa.GenerateKey(tt.curve, rand.Reader)
				require.NoError(t, err)
				require.NotNil(t, privKey)

				_, err = SerializeVerificationKey(&privKey.PublicKey)
				require.NoError(t, err)
			})

		t.Run(fmt.Sprintf("Serialize and Parse %s SigningKey", tt.name),
			func(t *testing.T) {
				t.Parallel()
				privateKey, err := ecdsa.GenerateKey(tt.curve, rand.Reader)
				require.NoError(t, err)
				key, err := SerializeSigningKey(privateKey)
				require.NotNil(t, key)
				require.NoError(t, err)

				_, err = ParseSigningKey(key)
				require.NoError(t, err)
			})
	}

	t.Run("Signing Key Empty", func(t *testing.T) {
		t.Parallel()
		_, err := SerializeSigningKey(&ecdsa.PrivateKey{})
		require.Error(t, err)
	})

	t.Run("Signing Key is nil", func(t *testing.T) {
		t.Parallel()
		_, err := SerializeSigningKey(nil)
		require.Error(t, err)
	})

	t.Run("Verification Key Empty", func(t *testing.T) {
		t.Parallel()
		_, err := SerializeVerificationKey(&ecdsa.PublicKey{})
		require.Error(t, err)
	})

	t.Run("Verification Key is nil", func(t *testing.T) {
		t.Parallel()
		_, err := SerializeVerificationKey(nil)
		require.Error(t, err)
	})
}
