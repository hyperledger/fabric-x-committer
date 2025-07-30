/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric/common/crypto/tlsgen"
)

// SecureCommunicationManager responsible for the creation of
// TLS certificates for testing purposes by utilizing the tls generation library of 'Hyperledger Fabric'.
// Path map convention: private-key, public-key, ca-certificate.
type SecureCommunicationManager struct {
	CertificateAuthority tlsgen.CA
}

// NewSecureCommunicationManager returns a SecureCommunicationManager with a new CA.
func NewSecureCommunicationManager(t *testing.T) *SecureCommunicationManager {
	t.Helper()
	ca, err := tlsgen.NewCA()
	require.NoError(t, err)
	return &SecureCommunicationManager{
		CertificateAuthority: ca,
	}
}

// CreateServerCertificate creates a server key pair given SAN (Subject Alternative Name),
// Writing it to a temp testing folder and returns a map with the credential paths.
func (scm *SecureCommunicationManager) CreateServerCertificate(
	t *testing.T,
	serverNameIndicator string,
) map[string]string {
	t.Helper()
	serverKeypair, err := scm.CertificateAuthority.NewServerCertKeyPair(serverNameIndicator)
	require.NoError(t, err)
	return createCertificatesPaths(t, createDataFromKeyPair(serverKeypair, scm.CertificateAuthority.CertBytes()))
}

// CreateClientCertificate creates a client key pair,
// Writing it to a temp testing folder and returns a map with the credential paths.
func (scm *SecureCommunicationManager) CreateClientCertificate(t *testing.T) map[string]string {
	t.Helper()
	clientKeypair, err := scm.CertificateAuthority.NewClientCertKeyPair()
	require.NoError(t, err)
	return createCertificatesPaths(t, createDataFromKeyPair(clientKeypair, scm.CertificateAuthority.CertBytes()))
}

func createCertificatesPaths(t *testing.T, data map[string][]byte) map[string]string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tmpDir))
	})

	paths := make(map[string]string)

	for key, value := range data {
		dataPath, err := saveBytesToFile(tmpDir, key, value)
		require.NoError(t, err)
		paths[key] = dataPath
	}
	return paths
}

func createDataFromKeyPair(keyPair *tlsgen.CertKeyPair, caCertificate []byte) map[string][]byte {
	data := make(map[string][]byte)
	data["private-key"] = keyPair.Key
	data["public-key"] = keyPair.Cert
	data["ca-certificate"] = caCertificate
	return data
}

func saveBytesToFile(dir, filename string, data []byte) (string, error) {
	filePath := filepath.Join(dir, filename)
	return filePath, os.WriteFile(filePath, data, 0o600)
}
