/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"crypto/tls"
	"crypto/x509"
	"os"

	"github.com/cockroachdb/errors"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// NewServerTLSConfig returns a TLS config to be used by a server.
func NewServerTLSConfig(c TLSConfig) (*tls.Config, error) {
	switch c.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return nil, nil
	case OneSideTLSMode, MutualTLSMode:
		tlsCfg := &tls.Config{
			MinVersion: DefaultTLSMinVersion,
			ClientAuth: tls.NoClientCert,
		}

		// Load server certificate and key pair (required for both modes)
		cert, err := tls.LoadX509KeyPair(c.CertPath, c.KeyPath)
		if err != nil {
			return nil, errors.Wrapf(err,
				"failed to load server certificate from %s or private key from %s", c.CertPath, c.KeyPath)
		}
		tlsCfg.Certificates = append(tlsCfg.Certificates, cert)

		// Load CA certificate pool (only for mutual TLS)
		if c.Mode == MutualTLSMode {
			tlsCfg.ClientCAs, err = buildCertPool(c.CACertPaths)
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

// NewClientTLSConfig returns a TLS config to be used by a client.
func NewClientTLSConfig(c TLSConfig) (*tls.Config, error) {
	switch c.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return nil, nil
	case OneSideTLSMode, MutualTLSMode:
		tlsCfg := &tls.Config{
			MinVersion: DefaultTLSMinVersion,
		}

		// Load client certificate and key pair (only for mutual TLS)
		if c.Mode == MutualTLSMode {
			cert, err := tls.LoadX509KeyPair(c.CertPath, c.KeyPath)
			if err != nil {
				return nil, errors.Wrapf(err,
					"failed to load client certificate from %s or private key from %s", c.CertPath, c.KeyPath)
			}
			tlsCfg.Certificates = append(tlsCfg.Certificates, cert)
		}

		// Load CA certificate pool (required for both modes)
		var err error
		tlsCfg.RootCAs, err = buildCertPool(c.CACertPaths)
		if err != nil {
			return nil, err
		}

		return tlsCfg, nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			c.Mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

// NewServerCredentials returns the gRPC transport credentials to be used by a server,
// based on the provided TLS configuration.
func NewServerCredentials(c TLSConfig) (credentials.TransportCredentials, error) {
	return newCredentials(NewServerTLSConfig(c))
}

// NewClientCredentials returns the gRPC transport credentials to be used by a client,
// based on the provided TLS configuration.
func NewClientCredentials(c TLSConfig) (credentials.TransportCredentials, error) {
	return newCredentials(NewClientTLSConfig(c))
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

func buildCertPool(paths []string) (*x509.CertPool, error) {
	if len(paths) == 0 {
		return nil, errors.New("no CA certificates provided")
	}
	certPool := x509.NewCertPool()
	for _, p := range paths {
		pemBytes, err := os.ReadFile(p)
		if err != nil {
			return nil, errors.Wrapf(err, "while reading CA cert %v", p)
		}
		if ok := certPool.AppendCertsFromPEM(pemBytes); !ok {
			return nil, errors.Errorf("unable to parse CA cert %v", p)
		}
	}
	return certPool, nil
}
