/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"

	"github.com/cockroachdb/errors"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// TLSMaterials holds the loaded runtime TLS material (certificate, key, CA certs).
type TLSMaterials struct {
	Mode    string
	Cert    []byte
	Key     []byte
	CACerts [][]byte
}

// CreateDynamicServerTLSConfig returns a TLS config with dynamic CA certificate support.
// It uses GetConfigForClient callback to retrieve the pre-configured tls.Config from services.
// This enables certificate rotation without service restart.
// Note: Only applies to MutualTLSMode. For other modes, returns standard server TLS config.
func (m *TLSMaterials) CreateDynamicServerTLSConfig(
	getDynamicFunc func(ctx context.Context) *tls.Config,
) (*tls.Config, error) {
	tlsConfig, err := m.CreateServerTLSConfig()
	if err != nil {
		return nil, errors.Newf("failed to create base server TLS config: %v", err)
	}
	if m.Mode != MutualTLSMode {
		return tlsConfig, nil
	}

	tlsConfig.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		// Load pre-configured tls.Config from service (atomic read, very fast)
		// Service has already merged static + dynamic CAs into ClientCAs
		cfg := getDynamicFunc(chi.Context())

		if cfg == nil {
			// Fallback to base config if service returns nil
			logger.Debugf("New client connection: %v, using base config", chi.Conn.RemoteAddr())
			return tlsConfig, nil
		}

		logger.Debugf("New client connection: %v, using service's pre-configured TLS config", chi.Conn.RemoteAddr())
		return cfg, nil
	}
	return tlsConfig, nil
}

// GetDynamicCACerts atomically reads and merges static and dynamic CA certificates.
// If dynamicRootCAs is nil, returns only the static CAs from YAML configuration.
// This allows services to opt-out of dynamic CA support by returning nil from GetDynamicRootCAs().
func (m *TLSMaterials) GetDynamicCACerts(dynamicRootCAs [][]byte) [][]byte {
	result := make([][]byte, len(m.CACerts)+len(dynamicRootCAs))
	copy(result, m.CACerts)
	copy(result[len(m.CACerts):], dynamicRootCAs)
	return result
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
func (m *TLSMaterials) CreateServerTLSConfig() (*tls.Config, error) {
	switch m.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return nil, nil
	case OneSideTLSMode, MutualTLSMode:
		tlsCfg := &tls.Config{
			MinVersion: DefaultTLSMinVersion,
			ClientAuth: tls.NoClientCert,
		}

		// Load server certificate and key pair (required for both modes)
		cert, err := tls.X509KeyPair(m.Cert, m.Key)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load server certificates")
		}
		tlsCfg.Certificates = append(tlsCfg.Certificates, cert)

		// Load CA certificate pool (only for mutual TLS)
		if m.Mode == MutualTLSMode {
			tlsCfg.ClientCAs, err = buildCertPool(m.CACerts)
			if err != nil {
				return nil, err
			}
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		}

		return tlsCfg, nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			m.Mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

// CreateClientTLSConfig returns a TLS config to be used by a server.
func (m *TLSMaterials) CreateClientTLSConfig() (*tls.Config, error) {
	switch m.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return nil, nil
	case OneSideTLSMode, MutualTLSMode:
		tlsCfg := &tls.Config{
			MinVersion: DefaultTLSMinVersion,
		}

		// Load client certificate and key pair (only for mutual TLS)
		if m.Mode == MutualTLSMode {
			cert, err := tls.X509KeyPair(m.Cert, m.Key)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to load client certificates")
			}
			tlsCfg.Certificates = append(tlsCfg.Certificates, cert)
		}

		// Load CA certificate pool (required for both modes)
		var err error
		tlsCfg.RootCAs, err = buildCertPool(m.CACerts)
		if err != nil {
			return nil, err
		}

		return tlsCfg, nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			m.Mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
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

// BuildCertPoolFromBytes builds an x509.CertPool from PEM-encoded certificate bytes.
// This is a public helper for services to pre-build their dynamic CertPools.
// Returns nil if rootCAs is empty or nil (allows services to opt-out of dynamic CAs).
func BuildCertPoolFromBytes(rootCAs [][]byte) (*x509.CertPool, error) {
	if len(rootCAs) == 0 {
		return nil, nil
	}
	return buildCertPool(rootCAs)
}

// MergeCACerts merges static and dynamic CA certificate bytes into a single slice.
// This is a helper for services to prepare CA bytes before building a CertPool.
func MergeCACerts(staticCAs, dynamicCAs [][]byte) [][]byte {
	if len(dynamicCAs) == 0 {
		return staticCAs
	}
	if len(staticCAs) == 0 {
		return dynamicCAs
	}

	merged := make([][]byte, 0, len(staticCAs)+len(dynamicCAs))
	merged = append(merged, staticCAs...)
	merged = append(merged, dynamicCAs...)
	return merged
}
