/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"sync/atomic"

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

// CreateDynamicServerTLSConfig returns a TLS config with dynamic CA certificate support.
// It uses GetConfigForClient callback to merge static CAs (from YAML) with dynamic CAs
// (from config blocks) on each TLS handshake.
// This enables certificate rotation without service restart.
// Note: Only applies to MutualTLSMode. For other modes, returns standard server TLS config.
func (m *TLSMaterials) CreateDynamicServerTLSConfig(
	getDynamicFunc func() *atomic.Pointer[[][]byte],
) (*tls.Config, error) {
	tlsConfig, err := m.CreateServerTLSConfig()
	if err != nil {
		return nil, errors.Newf("failed to create base server TLS config: %v", err)
	}
	if m.Mode != MutualTLSMode {
		return tlsConfig, nil
	}

	tlsConfig.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		caCerts := m.GetDynamicCACerts(getDynamicFunc())
		logger.Debugf("New client connection: %v, with %d root CAs", chi.Conn.RemoteAddr(), len(caCerts))
		certPool, err := buildCertPool(caCerts)
		if err != nil {
			return nil, errors.Wrap(err, "failed to build cert pool for client CAs")
		}
		cfg := &tls.Config{
			MinVersion:   DefaultTLSMinVersion,
			Certificates: tlsConfig.Certificates,
			ClientCAs:    certPool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}
		return cfg, nil
	}
	return tlsConfig, nil
}

// GetDynamicCACerts atomically reads and merges static and dynamic CA certificates.
// If dynamicRootCAs is nil, returns only the static CAs from YAML configuration.
// This allows services to opt-out of dynamic CA support by returning nil from GetDynamicRootCAs().
func (m *TLSMaterials) GetDynamicCACerts(dynamicRootCAs *atomic.Pointer[[][]byte]) [][]byte {
	if dynamicRootCAs == nil {
		return m.CACerts
	}

	if v := dynamicRootCAs.Load(); v != nil {
		result := make([][]byte, len(m.CACerts)+len(*v))
		copy(result, m.CACerts)
		copy(result[len(m.CACerts):], *v)
		return result
	}
	// Fallback to initial CAs
	return m.CACerts
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
