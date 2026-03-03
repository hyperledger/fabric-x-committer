/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/
package testsig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestParseSigningKey(t *testing.T) {
	t.Parallel()

	t.Run("parse EC PRIVATE KEY format", func(t *testing.T) {
		t.Parallel()
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)

		serialized, err := SerializeSigningKey(privateKey)
		require.NoError(t, err)

		parsed, err := ParseSigningKey(serialized)
		require.NoError(t, err)
		require.NotNil(t, parsed)
		require.Equal(t, privateKey.D, parsed.D)
	})

	t.Run("parse PRIVATE KEY (PKCS8) format", func(t *testing.T) {
		t.Parallel()
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)

		pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
		require.NoError(t, err)

		pemBytes := encodePEMBlock("PRIVATE KEY", pkcs8Bytes)

		parsed, err := ParseSigningKey(pemBytes)
		require.NoError(t, err)
		require.NotNil(t, parsed)
		require.Equal(t, privateKey.D, parsed.D)
	})

	t.Run("nil block returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ParseSigningKey([]byte("not a pem"))
		require.ErrorContains(t, err, "cannot decode PEM block content")
	})

	t.Run("unknown block type returns error", func(t *testing.T) {
		t.Parallel()
		pemBytes := encodePEMBlock("UNKNOWN TYPE", []byte("data"))

		_, err := ParseSigningKey(pemBytes)
		require.ErrorContains(t, err, "unknown block type")
	})

	t.Run("invalid PKCS8 data returns error", func(t *testing.T) {
		t.Parallel()
		pemBytes := encodePEMBlock("PRIVATE KEY", []byte("invalid data"))

		_, err := ParseSigningKey(pemBytes)
		require.ErrorContains(t, err, "cannot parse private key")
	})

	t.Run("invalid EC PRIVATE KEY data returns error", func(t *testing.T) {
		t.Parallel()
		pemBytes := encodePEMBlock("EC PRIVATE KEY", []byte("invalid data"))

		_, err := ParseSigningKey(pemBytes)
		require.ErrorContains(t, err, "cannot parse private key")
	})

	t.Run("non-ECDSA PKCS8 key returns error", func(t *testing.T) {
		t.Parallel()
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)

		pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(rsaKey)
		require.NoError(t, err)

		pemBytes := encodePEMBlock("PRIVATE KEY", pkcs8Bytes)

		_, err = ParseSigningKey(pemBytes)
		require.ErrorContains(t, err, "invalid private key")
	})
}

func TestGetSerializedKeyFromCert(t *testing.T) {
	t.Parallel()

	t.Run("successful extraction from certificate", func(t *testing.T) {
		t.Parallel()
		certPEM, _ := createTestCertificate(t)
		certPath := writeCertToTempFile(t, certPEM, "cert.pem")

		pubKey, err := GetSerializedKeyFromCert(certPath)
		require.NoError(t, err)
		require.NotNil(t, pubKey)
		require.NotEmpty(t, pubKey)
	})

	t.Run("non-existent file returns error", func(t *testing.T) {
		t.Parallel()
		_, err := GetSerializedKeyFromCert("/non/existent/path.pem")
		require.ErrorContains(t, err, "cannot read certificate")
	})

	t.Run("non-ECDSA certificate returns error", func(t *testing.T) {
		t.Parallel()
		certPEM := createRSACertificate(t)
		certPath := writeCertToTempFile(t, certPEM, "rsa-cert.pem")

		_, err := GetSerializedKeyFromCert(certPath)
		require.ErrorContains(t, err, "pubkey not ECDSA")
	})
}

func TestGetCert(t *testing.T) {
	t.Parallel()

	t.Run("successful certificate read", func(t *testing.T) {
		t.Parallel()
		certPEM, template := createTestCertificate(t)
		certPath := writeCertToTempFile(t, certPEM, "cert.pem")

		cert, err := GetCert(certPath)
		require.NoError(t, err)
		require.NotNil(t, cert)
		require.Equal(t, template.SerialNumber, cert.SerialNumber)
	})

	t.Run("non-existent file returns error", func(t *testing.T) {
		t.Parallel()
		_, err := GetCert("/non/existent/cert.pem")
		require.ErrorContains(t, err, "cannot read certificate")
	})
}

func TestGetCertFromBytes(t *testing.T) {
	t.Parallel()

	t.Run("successful certificate parsing", func(t *testing.T) {
		t.Parallel()
		certPEM, template := createTestCertificate(t)

		cert, err := GetCertFromBytes(certPEM)
		require.NoError(t, err)
		require.NotNil(t, cert)
		require.Equal(t, template.SerialNumber, cert.SerialNumber)
	})

	t.Run("no PEM content returns error", func(t *testing.T) {
		t.Parallel()
		_, err := GetCertFromBytes([]byte("not a pem"))
		require.ErrorContains(t, err, "no PEM content")
	})

	t.Run("invalid certificate data returns error", func(t *testing.T) {
		t.Parallel()
		pemBytes := encodePEMBlock("CERTIFICATE", []byte("invalid cert data"))

		_, err := GetCertFromBytes(pemBytes)
		require.ErrorContains(t, err, "cannot parse cert")
	})
}

// Helper functions.

// createTestCertificate creates a self-signed ECDSA certificate for testing.
func createTestCertificate(t *testing.T) ([]byte, *x509.Certificate) {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test Org"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)

	certPEM := encodePEMBlock("CERTIFICATE", certBytes)

	return certPEM, template
}

// createRSACertificate creates a self-signed RSA certificate for testing.
func createRSACertificate(t *testing.T) []byte {
	t.Helper()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test Org"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, template, template, &rsaKey.PublicKey, rsaKey)
	require.NoError(t, err)

	return encodePEMBlock("CERTIFICATE", certBytes)
}

// encodePEMBlock encodes data into a PEM block with the specified type.
func encodePEMBlock(blockType string, data []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  blockType,
		Bytes: data,
	})
}

// writeCertToTempFile writes certificate PEM data to a temporary file and returns the path.
func writeCertToTempFile(t *testing.T, certPEM []byte, filename string) string {
	t.Helper()

	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, filename)
	err := os.WriteFile(certPath, certPEM, 0o600)
	require.NoError(t, err)

	return certPath
}
