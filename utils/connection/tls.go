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

// NewClientCredentialsFromMaterial returns the gRPC transport credentials to be used by a client,
// based on the provided TLS configuration.
func NewClientCredentialsFromMaterial(c *TLSMaterials) (credentials.TransportCredentials, error) {
	return newCredentials(c.CreateClientTLSConfig())
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

// NewServerTLSMaterials converts a server TLSConfig with path fields into a struct
// that holds the actual bytes of the certificates.
//
// Certificate loading behavior by mode:
//   - none/unmentioned: No certificates loaded
//   - tls (one-way): Loads server cert and key only (CA certs NOT loaded)
//   - mtls (mutual): Loads server cert and key + CA certs for client verification
func NewServerTLSMaterials(c TLSConfig) (*TLSMaterials, error) {
	mode := c.Mode
	if mode == UnmentionedTLSMode {
		mode = DefaultTLSMode
	}
	materials := &TLSMaterials{Mode: mode}

	switch mode {
	case NoneTLSMode:
		return materials, nil

	case OneSideTLSMode, MutualTLSMode:
		var err error
		materials.Cert, err = os.ReadFile(c.CertPath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load certificate from %s", c.CertPath)
		}

		materials.Key, err = os.ReadFile(c.KeyPath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load private key from %s", c.KeyPath)
		}

		if mode == MutualTLSMode {
			materials.CACerts = make([][]byte, 0, len(c.CACertPaths))
			for _, path := range c.CACertPaths {
				caBytes, err := os.ReadFile(path)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to load root CA cert from %s", path)
				}
				materials.CACerts = append(materials.CACerts, caBytes)
			}
		}
		return materials, nil

	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid: %s, %s, %s)",
			mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

// NewClientTLSMaterials converts a client TLSConfig with path fields into a struct
// that holds the actual bytes of the certificates.
//
// Certificate loading behavior by mode:
//   - none/unmentioned: No certificates loaded
//   - tls (one-way): Loads CA certs only for server verification (client cert + key NOT loaded)
//   - mtls (mutual): Loads CA certs + client cert + key for mutual authentication
func NewClientTLSMaterials(c TLSConfig) (*TLSMaterials, error) {
	mode := c.Mode
	if mode == UnmentionedTLSMode {
		mode = DefaultTLSMode
	}
	materials := &TLSMaterials{Mode: mode}

	switch mode {
	case NoneTLSMode:
		return materials, nil

	case OneSideTLSMode, MutualTLSMode:
		materials.CACerts = make([][]byte, 0, len(c.CACertPaths))
		for _, path := range c.CACertPaths {
			caBytes, err := os.ReadFile(path)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to load root CA cert from %s", path)
			}
			materials.CACerts = append(materials.CACerts, caBytes)
		}

		if mode == MutualTLSMode {
			var err error
			materials.Cert, err = os.ReadFile(c.CertPath)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to load client certificate from %s", c.CertPath)
			}

			materials.Key, err = os.ReadFile(c.KeyPath)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to load client private key from %s", c.KeyPath)
			}
		}
		return materials, nil

	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid: %s, %s, %s)",
			mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

// CreateServerTLSConfig returns a TLS config for the server.
// If getTLSConfigFunc is provided and mode is MutualTLS, it enables dynamic CA certificate support
// using GetConfigForClient callback.
// This enables certificate rotation without a service restart.
// Pass nil for getTLSConfigFunc to use static configuration only.
func (m *TLSMaterials) CreateServerTLSConfig(
	getTLSConfigFunc func(ctx context.Context) *tls.Config,
) (*tls.Config, error) {
	tlsConfig, err := m.createBasicServerTLSConfig()
	if err != nil {
		return nil, errors.Newf("failed to create base server TLS config: %v", err)
	}

	// Only enable dynamic CA support for MutualTLS mode when a function is provided.
	if m.Mode == MutualTLSMode && getTLSConfigFunc != nil {
		tlsConfig.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			// Load pre-configured tls.Config from service.
			// The Service has already merged static + dynamic CAs into ClientCAs
			cfg := getTLSConfigFunc(chi.Context())

			if cfg == nil {
				// Fallback to base config if service returns nil
				logger.Debugf("Client connection: %v, using base config", chi.Conn.RemoteAddr())
				return tlsConfig, nil
			}

			logger.Debugf("Client connection: %v, using pre-configured config", chi.Conn.RemoteAddr())
			return cfg, nil
		}
	}
	return tlsConfig, nil
}

// createBasicServerTLSConfig creates the base server TLS configuration without dynamic CA support.
// This is an internal helper used by CreateServerTLSConfig.
func (m *TLSMaterials) createBasicServerTLSConfig() (*tls.Config, error) {
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
			tlsCfg.ClientCAs, err = BuildCertPool(m.CACerts)
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
		tlsCfg.RootCAs, err = BuildCertPool(m.CACerts)
		if err != nil {
			return nil, err
		}

		return tlsCfg, nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			m.Mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

// BuildCertPool creates a x509.CertPool from a slice of CA certificate bytes.
func BuildCertPool(rootCAs [][]byte) (*x509.CertPool, error) {
	if len(rootCAs) == 0 {
		return nil, errors.New("no CA certificates provided")
	}
	certPool := x509.NewCertPool()
	for _, rootCA := range rootCAs {
		if ok := certPool.AppendCertsFromPEM(rootCA); !ok {
			return nil, errors.Errorf("failed to parse CA cert")
		}
	}
	return certPool, nil
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
