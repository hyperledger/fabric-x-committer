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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type (
	// ConfigTLS holds the TLS options and certificate paths
	// for secure communication between servers and clients.
	ConfigTLS struct {
		Mode TLSMode `mapstructure:"tls-mode"`
		// ServerName is required by the client if the server's certificate uses SNI.
		ServerName  string   `mapstructure:"server-name"`
		CertPath    string   `mapstructure:"cert-path"`
		KeyPath     string   `mapstructure:"key-path"`
		CACertPaths []string `mapstructure:"ca-cert-paths"`
	}

	// TLSMode defines the desired level of TLS security.
	TLSMode string
)

const (
	//nolint:revive // usage: TLS configuration modes.
	TLSEmpty  TLSMode = ""
	TLSNone   TLSMode = "none"
	TLSServer TLSMode = "tls"
	TLSMutual TLSMode = "mtls"
)

// ServerOption returns the appropriate gRPC server option based on the TLS configuration.
// If TLS is enabled, it returns a server option with TLS credentials; otherwise,
// it returns an insecure option.
//
//nolint:ireturn // returning grpc.ServerOption interface is intentional for abstraction
func (c *ConfigTLS) ServerOption() (grpc.ServerOption, error) {
	if c == nil {
		return grpc.Creds(insecure.NewCredentials()), nil
	}
	creds, err := c.buildServerCreds()
	if err != nil {
		return nil, err
	}
	return grpc.Creds(creds), nil
}

// ClientOption returns the gRPC transport credentials to be used by a client,
// based on the provided TLS configuration.
// If TLS is disabled or c is nil, it returns
// insecure credentials; otherwise, it returns TLS credentials configured
// with or without mutual TLS, depending on the settings.
func (c *ConfigTLS) ClientOption() (credentials.TransportCredentials, error) {
	if c == nil {
		return insecure.NewCredentials(), nil
	}
	_, creds, err := c.buildClientCreds()
	return creds, err
}

// ClientOptionWithConfig returns the gRPC transport credentials and
// the tls configuration to be used by a client,
// based on the provided TLS configuration.
// If TLS is disabled or c is nil, it returns
// insecure credentials; otherwise, it returns TLS credentials configured
// with or without mutual TLS, depending on the settings.
func (c *ConfigTLS) ClientOptionWithConfig() (*tls.Config, credentials.TransportCredentials, error) {
	if c == nil {
		return nil, insecure.NewCredentials(), nil
	}
	return c.buildClientCreds()
}

func (c *ConfigTLS) buildServerCreds() (credentials.TransportCredentials, error) {
	switch c.Mode {
	case TLSNone, TLSEmpty:
		return insecure.NewCredentials(), nil

	case TLSServer, TLSMutual:
		cert, err := tls.LoadX509KeyPair(c.CertPath, c.KeyPath)
		if err != nil {
			return nil, errors.Wrapf(err, "while loading server certificate and private key")
		}

		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			ClientAuth:   tls.NoClientCert,
		}

		if c.Mode == TLSMutual {
			certPool, err := buildCertPool(c.CACertPaths)
			if err != nil {
				return nil, err
			}
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
			tlsCfg.ClientCAs = certPool
		}

		return credentials.NewTLS(tlsCfg), nil

	default:
		return nil, errors.Errorf("unknown tls mode %v", c.Mode)
	}
}

func (c *ConfigTLS) buildClientCreds() (*tls.Config, credentials.TransportCredentials, error) {
	switch c.Mode {
	case TLSNone, TLSEmpty:
		return nil, insecure.NewCredentials(), nil

	case TLSServer, TLSMutual:
		certPool, err := buildCertPool(c.CACertPaths)
		if err != nil {
			return nil, nil, err
		}

		tlsCfg := &tls.Config{
			RootCAs:    certPool,
			MinVersion: tls.VersionTLS12,
		}

		if c.Mode == TLSMutual {
			cert, err := tls.LoadX509KeyPair(c.CertPath, c.KeyPath)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "while loading client certificate and private key")
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}

		if c.ServerName != "" {
			tlsCfg.ServerName = c.ServerName
		}

		return tlsCfg, credentials.NewTLS(tlsCfg), nil

	default:
		return nil, nil, errors.Errorf("unknown tls mode: %v", c.Mode)
	}
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
