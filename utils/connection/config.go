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
	MultiClientConfig struct {
		Endpoints []*Endpoint   `mapstructure:"endpoints" yaml:"endpoints"`
		TLS       *TLSConfig    `mapstructure:"tls"       yaml:"tls"`
		Retry     *RetryProfile `mapstructure:"reconnect" yaml:"reconnect"`
	}

	// ClientConfig contains a single endpoint, CAs, and retry profile.
	ClientConfig struct {
		Endpoint *Endpoint     `mapstructure:"endpoint"  yaml:"endpoint"`
		TLS      *TLSConfig    `mapstructure:"tls"       yaml:"tls"`
		Retry    *RetryProfile `mapstructure:"reconnect" yaml:"reconnect"`
	}

	// ServerConfig describes the connection parameter for a server.
	ServerConfig struct {
		Endpoint  Endpoint               `mapstructure:"endpoint"`
		TLS       *TLSConfig             `mapstructure:"tls"`
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
	//
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
)

const (
	//nolint:revive // usage: TLS configuration modes.
	UnmentionedTLSMode = ""
	NoneTLSMode        = "none"
	ServerSideTLSMode  = "tls"
	MutualTLSMode      = "mtls"

	// DefaultTLSMinVersion is the minimum version required to achieve secure connections.
	DefaultTLSMinVersion = tls.VersionTLS12
)

// ServerCredentials returns the appropriate gRPC server option based on the TLS configuration.
// If TLS is enabled, it returns a server option with TLS credentials; otherwise,
// it returns an insecure option.
func (c *TLSConfig) ServerCredentials() (credentials.TransportCredentials, error) {
	if c == nil {
		return insecure.NewCredentials(), nil
	}
	return c.buildServerCreds()
}

// ClientCredentials returns the gRPC transport credentials to be used by a client,
// based on the provided TLS configuration.
// If TLS is disabled or c is nil, it returns
// insecure credentials; otherwise, it returns TLS credentials configured
// with or without mutual TLS, depending on the settings.
func (c *TLSConfig) ClientCredentials() (credentials.TransportCredentials, error) {
	if c == nil {
		return insecure.NewCredentials(), nil
	}
	return c.buildClientCreds()
}

func (c *TLSConfig) buildServerCreds() (credentials.TransportCredentials, error) {
	switch c.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return insecure.NewCredentials(), nil
	case ServerSideTLSMode, MutualTLSMode:
		// In server-side or mutual TLS, the server must load its certificate and key pair.
		// The client uses this certificate chain to authenticate the server.
		cert, err := tls.LoadX509KeyPair(c.CertPath, c.KeyPath)
		if err != nil {
			return nil, errors.Wrapf(err,
				"failed to load server certificate from %s and private key from %s", c.CertPath, c.KeyPath)
		}

		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   DefaultTLSMinVersion,
			ClientAuth:   tls.NoClientCert,
		}

		// In mutual TLS, the server must also load the trusted CA(s) to validate client certificates.
		if c.Mode == MutualTLSMode {
			certPool, err := buildCertPool(c.CACertPaths)
			if err != nil {
				return nil, errors.Wrap(err, "failed to build CA certificate pool")
			}
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
			tlsCfg.ClientCAs = certPool
		}

		return credentials.NewTLS(tlsCfg), nil
	default:
		return nil, errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			c.Mode, NoneTLSMode, ServerSideTLSMode, MutualTLSMode)
	}
}

func (c *TLSConfig) buildClientCreds() (credentials.TransportCredentials, error) {
	switch c.Mode {
	case NoneTLSMode, UnmentionedTLSMode:
		return insecure.NewCredentials(), nil

	case ServerSideTLSMode, MutualTLSMode:
		// For both server-side TLS and mutual TLS, the client must load the trusted CA(s)
		// used to validate the server's certificate.
		certPool, err := buildCertPool(c.CACertPaths)
		if err != nil {
			return nil, errors.Wrap(err, "failed to build CA certificate pool")
		}

		tlsCfg := &tls.Config{
			RootCAs:    certPool,
			MinVersion: DefaultTLSMinVersion,
		}

		// In mutual TLS, the client must also present its own certificate and key pair
		// so the server can authenticate the client.
		if c.Mode == MutualTLSMode {
			cert, err := tls.LoadX509KeyPair(c.CertPath, c.KeyPath)
			if err != nil {
				return nil, errors.Wrapf(err,
					"failed to load client certificate from %s and private key from %s", c.CertPath, c.KeyPath)
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
		return credentials.NewTLS(tlsCfg), nil

	default:
		return nil, errors.Errorf("unknown tls mode: %v", c.Mode)
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
