/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"time"

	"github.com/cockroachdb/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type (
	// ClientConfig contains the endpoints, CAs, and retry profile.
	ClientConfig struct {
		Endpoints []*Endpoint   `mapstructure:"endpoints"`
		Creds     *TLSConfig    `mapstructure:"creds"`
		Retry     *RetryProfile `mapstructure:"reconnect"`
	}

	// ServerConfig describes the connection parameter for a server.
	ServerConfig struct {
		Endpoint  Endpoint               `mapstructure:"endpoint"`
		Creds     *TLSConfig             `mapstructure:"creds"`
		KeepAlive *ServerKeepAliveConfig `mapstructure:"keep-alive"`

		preAllocatedListener net.Listener
	}

	// ServerKeepAliveConfig describes the keep alive parameters.
	ServerKeepAliveConfig struct {
		Params            *ServerKeepAliveParamsConfig            `mapstructure:"params"`
		EnforcementPolicy *ServerKeepAliveEnforcementPolicyConfig `mapstructure:"enforcement-policy"`
	}

	// ServerKeepAliveParamsConfig describes the keep alive policy.
	ServerKeepAliveParamsConfig struct {
		MaxConnectionIdle     time.Duration `mapstructure:"max-connection-idle"`
		MaxConnectionAge      time.Duration `mapstructure:"max-connection-age"`
		MaxConnectionAgeGrace time.Duration `mapstructure:"max-connection-age-grace"`
		Time                  time.Duration `mapstructure:"time"`
		Timeout               time.Duration `mapstructure:"timeout"`
	}

	// ServerKeepAliveEnforcementPolicyConfig describes the keep alive enforcement policy.
	ServerKeepAliveEnforcementPolicyConfig struct {
		MinTime             time.Duration `mapstructure:"min-time"`
		PermitWithoutStream bool          `mapstructure:"permit-without-stream"`
	}

	// TLSConfig holds the TLS options and certificate paths
	// for secure communication between servers and clients.
	TLSConfig struct {
		Mode string `mapstructure:"tls-mode"`
		// ServerName is required by the client if the server's certificate uses SNI.
		ServerName  string   `mapstructure:"server-name"`
		CertPath    string   `mapstructure:"cert-path"`
		KeyPath     string   `mapstructure:"key-path"`
		CACertPaths []string `mapstructure:"ca-cert-paths"`
	}
)

const (
	//nolint:revive // usage: TLS configuration modes.
	DefaultTLSMode    = ""
	NoneTLSMode       = "none"
	ServerSideTLSMode = "tls"
	MutualTLSMode     = "mtls"
)

// ServerOption returns the appropriate gRPC server option based on the TLS configuration.
// If TLS is enabled, it returns a server option with TLS credentials; otherwise,
// it returns an insecure option.
//
//nolint:ireturn // returning grpc.ServerOption interface is intentional for abstraction
func (c *TLSConfig) ServerOption() (grpc.ServerOption, error) {
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
func (c *TLSConfig) ClientOption() (credentials.TransportCredentials, error) {
	if c == nil {
		return insecure.NewCredentials(), nil
	}
	_, creds, err := c.buildClientCreds()
	return creds, err
}

func (c *TLSConfig) buildServerCreds() (credentials.TransportCredentials, error) {
	switch c.Mode {
	case NoneTLSMode, DefaultTLSMode:
		return insecure.NewCredentials(), nil

	case ServerSideTLSMode, MutualTLSMode:
		cert, err := tls.LoadX509KeyPair(c.CertPath, c.KeyPath)
		if err != nil {
			return nil, errors.Wrapf(err, "while loading server certificate and private key")
		}

		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			ClientAuth:   tls.NoClientCert,
		}

		if c.Mode == MutualTLSMode {
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

func (c *TLSConfig) buildClientCreds() (*tls.Config, credentials.TransportCredentials, error) {
	switch c.Mode {
	case NoneTLSMode, DefaultTLSMode:
		return nil, insecure.NewCredentials(), nil

	case ServerSideTLSMode, MutualTLSMode:
		certPool, err := buildCertPool(c.CACertPaths)
		if err != nil {
			return nil, nil, err
		}

		tlsCfg := &tls.Config{
			RootCAs:    certPool,
			MinVersion: tls.VersionTLS12,
		}

		if c.Mode == MutualTLSMode {
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
