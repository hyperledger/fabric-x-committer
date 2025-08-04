/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package tls

import (
	"crypto/tls"
	"crypto/x509"
	"os"

	"github.com/cockroachdb/errors"
)

// LoadTLSCredentials returns a new [tls.Config] with the credentials loaded from the given paths.
func LoadTLSCredentials(certPaths []string) (*tls.Config, error) {
	certs := make([][]byte, len(certPaths))
	var err error
	for i, p := range certPaths {
		// Load certificate of the CA who signed server's certificate
		certs[i], err = os.ReadFile(p)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to credentials from file: %s", p)
		}
	}
	return LoadTLSCredentialsRaw(certs)
}

// LoadTLSCredentialsRaw returns a new [tls.Config] with the credentials from the given bytes.
func LoadTLSCredentialsRaw(certs [][]byte) (*tls.Config, error) {
	if len(certs) < 1 {
		return nil, errors.New("no ROOT CAS")
	}

	certPool := x509.NewCertPool()
	for _, cert := range certs {
		if !certPool.AppendCertsFromPEM(cert) {
			return nil, errors.New("failed to add server CA's certificate")
		}
	}

	// Create the credentials and return it
	return &tls.Config{
		RootCAs:    certPool,
		MinVersion: tls.VersionTLS12,
	}, nil
}
