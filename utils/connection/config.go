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
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type (
	// MultiClientConfig contains the endpoints, CAs, and retry profile.
	// This config allows the support of number of different endpoints to multiple service instances.
	MultiClientConfig struct {
		Endpoints []*Endpoint   `mapstructure:"endpoints" yaml:"endpoints"`
		TLS       TLSConfig     `mapstructure:"tls"       yaml:"tls"`
		Retry     *RetryProfile `mapstructure:"reconnect" yaml:"reconnect"`
	}

	// ClientConfig contains a single endpoint, CAs, and retry profile.
	ClientConfig struct {
		Endpoint *Endpoint     `mapstructure:"endpoint"  yaml:"endpoint"`
		TLS      TLSConfig     `mapstructure:"tls"       yaml:"tls"`
		Retry    *RetryProfile `mapstructure:"reconnect" yaml:"reconnect"`
	}

	// ServerConfig describes the connection parameter for a server.
	ServerConfig struct {
		Endpoint  Endpoint               `mapstructure:"endpoint"`
		TLS       TLSConfig              `mapstructure:"tls"`
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
	// used for secure communication between servers and clients.
	// Credentials are built based on the configuration mode.
	// For example, If only server-side TLS is required, the certificate pool (certPool) is not built (for a server),
	// since the relevant certificates paths are defined in the YAML according to the selected mode.
	TLSConfig struct {
		Mode string `mapstructure:"mode"`
		// CertPath is the path to the certificate file (public key).
		CertPath string `mapstructure:"cert-path"`
		// KeyPath is the path to the key file (private key).
		KeyPath     string   `mapstructure:"key-path"`
		CACertPaths []string `mapstructure:"ca-cert-paths"`
	}

	// DatabaseTLSConfig holds the database TLS mode and its necessary credentials.
	DatabaseTLSConfig struct {
		Mode       string `mapstructure:"mode"`
		CACertPath string `mapstructure:"ca-cert-path"`
	}
)

const (
	//nolint:revive // usage: TLS configuration modes.
	UnmentionedTLSMode = ""
	NoneTLSMode        = "none"
	OneSideTLSMode     = "tls"
	MutualTLSMode      = "mtls"

	// DefaultTLSMinVersion is the minimum version required to achieve secure connections.
	DefaultTLSMinVersion = tls.VersionTLS12
)

// ServerCredentials returns the gRPC transport credentials to be used by a server,
// based on the provided TLS configuration.
func (c TLSConfig) ServerCredentials() (credentials.TransportCredentials, error) {
	switch c.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return insecure.NewCredentials(), nil
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

		return credentials.NewTLS(tlsCfg), nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			c.Mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
	}
}

// ClientCredentials returns the gRPC transport credentials to be used by a client,
// based on the provided TLS configuration.
func (c TLSConfig) ClientCredentials() (credentials.TransportCredentials, error) {
	switch c.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return insecure.NewCredentials(), nil
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

		return credentials.NewTLS(tlsCfg), nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			c.Mode, NoneTLSMode, OneSideTLSMode, MutualTLSMode)
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
