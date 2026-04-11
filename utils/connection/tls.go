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

type (
	// DynamicTLSService is an optional interface for services that support dynamic CA certificate updates.
	// Services implementing this interface can refresh their TLS configuration without restart.
	DynamicTLSService interface {
		// GetTLSConfig returns a pre-configured tls.Config with merged static + dynamic CAs.
		// Returns nil if the service doesn't support dynamic CAs or TLS is not configured.
		GetTLSConfig(ctx context.Context) *tls.Config
	}
	// TLSCredentials holds the loaded runtime TLS credentials (certificate, key, CA certs).
	TLSCredentials struct {
		Mode    string
		Cert    []byte
		Key     []byte
		CACerts [][]byte
	}
)

// NewClientGRPCTransportCredentials returns the gRPC transport credentials to be used by a client,
// based on the provided TLS credentials.
func NewClientGRPCTransportCredentials(c *TLSCredentials) (credentials.TransportCredentials, error) {
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

// NewServerTLSCredentials converts a server TLSConfig with path fields into a struct
// that holds the actual bytes of the certificates.
//
// Certificate loading behavior by mode:
//   - none/unmentioned: No certificates loaded
//   - tls (one-way): Loads server cert + key only (CA certs NOT loaded)
//   - mtls (mutual): Loads server cert + key + CA certs for client verification
func NewServerTLSCredentials(c TLSConfig) (*TLSCredentials, error) {
	mode := c.Mode
	if mode == UnmentionedTLSMode {
		mode = DefaultTLSMode
	}
	creds := &TLSCredentials{Mode: mode}

	switch mode {
	case NoneTLSMode:
		return creds, nil

	case OneSideTLSMode, MutualTLSMode:
		var err error
		creds.Cert, err = os.ReadFile(c.CertPath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load certificate from %s", c.CertPath)
		}

		creds.Key, err = os.ReadFile(c.KeyPath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load private key from %s", c.KeyPath)
		}

		if mode == MutualTLSMode {
			creds.CACerts = make([][]byte, 0, len(c.CACertPaths))
			for _, path := range c.CACertPaths {
				caBytes, err := os.ReadFile(path)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to load root CA cert from %s", path)
				}
				creds.CACerts = append(creds.CACerts, caBytes)
			}
		}
		return creds, nil

	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid: %s, %s, %s)",
			mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

// NewClientTLSCredentials converts a client TLSConfig with path fields into a struct
// that holds the actual bytes of the certificates.
//
// Certificate loading behavior by mode:
//   - none/unmentioned: No certificates loaded
//   - tls (one-way): Loads CA certs only for server verification (client cert + key NOT loaded)
//   - mtls (mutual): Loads CA certs + client cert + key for mutual authentication
func NewClientTLSCredentials(c TLSConfig) (*TLSCredentials, error) {
	mode := c.Mode
	if mode == UnmentionedTLSMode {
		mode = DefaultTLSMode
	}
	creds := &TLSCredentials{Mode: mode}

	switch mode {
	case NoneTLSMode:
		return creds, nil

	case OneSideTLSMode, MutualTLSMode:
		creds.CACerts = make([][]byte, 0, len(c.CACertPaths))
		for _, path := range c.CACertPaths {
			caBytes, err := os.ReadFile(path)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to load root CA cert from %s", path)
			}
			creds.CACerts = append(creds.CACerts, caBytes)
		}

		if mode == MutualTLSMode {
			var err error
			creds.Cert, err = os.ReadFile(c.CertPath)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to load client certificate from %s", c.CertPath)
			}

			creds.Key, err = os.ReadFile(c.KeyPath)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to load client private key from %s", c.KeyPath)
			}
		}
		return creds, nil

	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid: %s, %s, %s)",
			mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

// CreateServerTLSConfig returns a TLS config for the server.
// If dynamicService is provided and mode is MutualTLS, it enables dynamic CA certificate support
// using GetConfigForClient callback.
// This enables certificate rotation without a service restart.
func (c *TLSCredentials) CreateServerTLSConfig(dynamicService DynamicTLSService) (*tls.Config, error) {
	tlsConfig, err := c.CreateStaticTLSConfig()
	if err != nil {
		return nil, errors.Newf("failed to create base server TLS config: %v", err)
	}

	// Only enable dynamic CA support for MutualTLS mode and if dynamicService is provided.
	if c.Mode == MutualTLSMode && dynamicService != nil {
		tlsConfig.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			// Load pre-configured tls.Config from service.
			// The Service has already merged static + dynamic CAs into ClientCAs
			cfg := dynamicService.GetTLSConfig(chi.Context())

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

// CreateStaticTLSConfig creates a static server TLS configuration without dynamic CA support.
func (c *TLSCredentials) CreateStaticTLSConfig() (*tls.Config, error) {
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
			tlsCfg.ClientCAs, err = BuildCertPool(c.CACerts)
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
func (c *TLSCredentials) CreateClientTLSConfig() (*tls.Config, error) {
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
		tlsCfg.RootCAs, err = BuildCertPool(c.CACerts)
		if err != nil {
			return nil, err
		}

		return tlsCfg, nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			c.Mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
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
			return nil, errors.Errorf("unable to parse CA cert")
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
