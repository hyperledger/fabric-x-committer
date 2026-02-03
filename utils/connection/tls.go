/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"crypto/tls"
	"crypto/x509"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cockroachdb/errors"
)

// TLSMaterials holds the loaded runtime TLS material (certificate, key, CA certs).
type TLSMaterials struct {
	Mode    string
	Cert    []byte
	Key     []byte
	CACerts [][]byte
}

// NewClientCredentialsFromMaterial returns the gRPC transport credentials to be used by a client,
// based on the provided TLS configuration.
func NewClientCredentialsFromMaterial(c *TLSMaterials) (credentials.TransportCredentials, error) {
	return newCredentials(c.CreateClientTLSConfig())
}

// NewServerCredentialsFromMaterial returns the gRPC transport credentials to be used by a client,
// based on the provided TLS configuration.
func NewServerCredentialsFromMaterial(c *TLSMaterials) (credentials.TransportCredentials, error) {
	return newCredentials(c.CreateServerTLSConfig())
}

func newCredentials(tlsCfg *tls.Config, err error) (credentials.TransportCredentials, error) {
	if err != nil {
		return nil, err
	}
	if tlsCfg == nil {
		return insecure.NewCredentials(), nil
	}
	return credentials.NewTLS(tlsCfg), nil
}

// NewTLSMaterials converts a TLSConfig with path fields into a struct that holds the actual bytes of the certificates.
func NewTLSMaterials(c TLSConfig) (*TLSMaterials, error) {
	if c.Mode == NoneTLSMode || c.Mode == UnmentionedTLSMode {
		return &TLSMaterials{
			Mode: c.Mode,
		}, nil
	}

	certBytes, err := os.ReadFile(c.CertPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load certificate from %s", c.CertPath)
	}

	keyBytes, err := os.ReadFile(c.KeyPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load private key from %s", c.KeyPath)
	}

	caCertBytes := make([][]byte, 0, len(c.CACertPaths))
	for _, caCertPath := range c.CACertPaths {
		caBytes, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load root CA cert from %s", caCertPath)
		}
		caCertBytes = append(caCertBytes, caBytes)
	}

	return &TLSMaterials{
		Mode:    c.Mode,
		Cert:    certBytes,
		Key:     keyBytes,
		CACerts: caCertBytes,
	}, nil
}

// CreateServerTLSConfig returns a TLS config to be used by a server.
func (c *TLSMaterials) CreateServerTLSConfig() (*tls.Config, error) {
	switch c.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return nil, nil
	case OneSideTLSMode, MutualTLSMode:
		tlsCfg := &tls.Config{
			MinVersion: DefaultTLSMinVersion,
			ClientAuth: tls.NoClientCert,
		}

		// Load server certificate and key pair (required for both modes)
		cert, err := tls.X509KeyPair(c.Cert, c.Key)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load server certificates")
		}
		tlsCfg.Certificates = append(tlsCfg.Certificates, cert)

		// Load CA certificate pool (only for mutual TLS)
		if c.Mode == MutualTLSMode {
			tlsCfg.ClientCAs, err = buildCertPool(c.CACerts)
			if err != nil {
				return nil, err
			}
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		}

		return tlsCfg, nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			c.Mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

// CreateClientTLSConfig returns a TLS config to be used by a server.
func (c *TLSMaterials) CreateClientTLSConfig() (*tls.Config, error) {
	switch c.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return nil, nil
	case OneSideTLSMode, MutualTLSMode:
		tlsCfg := &tls.Config{
			MinVersion: DefaultTLSMinVersion,
		}

		// Load client certificate and key pair (only for mutual TLS)
		if c.Mode == MutualTLSMode {
			cert, err := tls.X509KeyPair(c.Cert, c.Key)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to load client certificates")
			}
			tlsCfg.Certificates = append(tlsCfg.Certificates, cert)
		}

		// Load CA certificate pool (required for both modes)
		var err error
		tlsCfg.RootCAs, err = buildCertPool(c.CACerts)
		if err != nil {
			return nil, err
		}

		return tlsCfg, nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			c.Mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

func buildCertPool(rootCAs [][]byte) (*x509.CertPool, error) {
	if len(rootCAs) == 0 {
		return nil, errors.New("no CA certificates provided")
	}
	certPool := x509.NewCertPool()
	for _, rootCA := range rootCAs {
		if ok := certPool.AppendCertsFromPEM(rootCA); !ok {
			return nil, errors.Errorf("unable to parse CA cert")
		}
	}
	return certPool, nil
}
