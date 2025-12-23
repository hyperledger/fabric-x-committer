/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyperledger/fabric-lib-go/bccsp"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/sha3"
)

func TestDigest(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	validCertPath := filepath.Join(tempDir, "valid.pem")
	certDerBytes := generateSelfSignedCert(t, validCertPath)

	expectedHashSHA256 := sha256.Sum256(certDerBytes)
	expectedHashSHA384 := sha512.Sum384(certDerBytes)
	// sha3.Sum256 and Sum384 return arrays, we need slices for comparison or direct array comparison
	h3_256 := sha3.New256()
	h3_256.Write(certDerBytes)
	expectedHashSHA3_256 := h3_256.Sum(nil)

	invalidPemPath := filepath.Join(tempDir, "garbage.pem")
	require.NoError(t, os.WriteFile(invalidPemPath, []byte("Not a PEM file"), 0o644))

	tests := []struct {
		name          string
		filePath      string
		hashFunc      string
		expectedHash  []byte
		expectedError string
	}{
		{
			name:         "Success SHA256",
			filePath:     validCertPath,
			hashFunc:     bccsp.SHA256,
			expectedHash: expectedHashSHA256[:],
		},
		{
			name:         "Success SHA384",
			filePath:     validCertPath,
			hashFunc:     bccsp.SHA384,
			expectedHash: expectedHashSHA384[:],
		},
		{
			name:         "Success SHA3_256",
			filePath:     validCertPath,
			hashFunc:     bccsp.SHA3_256,
			expectedHash: expectedHashSHA3_256,
		},
		{
			name:          "File Not Found",
			filePath:      filepath.Join(tempDir, "non_existent.pem"),
			hashFunc:      bccsp.SHA256,
			expectedError: "cannot read certificate",
		},
		{
			name:          "Invalid PEM Content",
			filePath:      invalidPemPath,
			hashFunc:      bccsp.SHA256,
			expectedError: "no PEM content in file",
		},
		{
			name:          "Unsupported Hash Function",
			filePath:      validCertPath,
			hashFunc:      "MD5",
			expectedError: "unsupported hash function",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			digest, err := Digest(tt.filePath, tt.hashFunc)

			if tt.expectedError != "" {
				require.ErrorContains(t, err, tt.expectedError)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tt.expectedHash, digest)
		})
	}
}

func generateSelfSignedCert(t *testing.T, path string) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test Org"},
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(time.Hour),
		IsCA:      true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	require.NoError(t, err)

	pemFile, err := os.Create(path)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, pemFile.Close())
	}()

	require.NoError(t, pem.Encode(pemFile, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}))
	return derBytes
}
