/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"crypto/tls"
	"net"
	"time"

	"github.com/cockroachdb/errors"

	"google.golang.org/grpc/credentials"
)

type (
	// MultiClientConfig contains the endpoints, TLS config, and retry profile.
	// This config allows the support of number of different endpoints to multiple service instances.
	MultiClientConfig struct {
		Endpoints []*Endpoint   `mapstructure:"endpoints" yaml:"endpoints"`
		TLS       TLSConfig     `mapstructure:"tls"       yaml:"tls"`
		Retry     *RetryProfile `mapstructure:"reconnect" yaml:"reconnect"`
	}

	// ClientConfig contains a single endpoint, TLS config, and retry profile.
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
		RateLimit RateLimitConfig        `mapstructure:"rate-limit"`

		preAllocatedListener net.Listener
	}

	// RateLimitConfig describes the rate limiting configuration for unary gRPC endpoints.
	RateLimitConfig struct {
		// RequestsPerSecond is the maximum number of requests per second allowed.
		// Set to 0 or negative to disable rate limiting.
		RequestsPerSecond int `mapstructure:"requests-per-second"`
		// Burst is the maximum number of requests allowed in a single burst.
		// This allows handling sudden spikes of concurrent requests from multiple clients.
		// Must be greater than 0 and less than or equal to RequestsPerSecond when rate limiting is enabled.
		Burst int `mapstructure:"burst"`
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

// ClientCredentials converts TLSConfig into a TLSMaterials struct and generates client creds.
func (c TLSConfig) ClientCredentials() (credentials.TransportCredentials, error) {
	tlsMaterials, err := NewTLSMaterials(c)
	if err != nil {
		return nil, err
	}
	return NewClientCredentialsFromMaterial(tlsMaterials)
}

// ServerCredentials converts TLSConfig into a TLSMaterials struct and generates server creds.
func (c TLSConfig) ServerCredentials() (credentials.TransportCredentials, error) {
	tlsMaterials, err := NewTLSMaterials(c)
	if err != nil {
		return nil, err
	}
	return NewServerCredentialsFromMaterial(tlsMaterials)
}

// Validate checks that the rate limit configuration is valid.
func (c *RateLimitConfig) Validate() error {
	if c == nil || c.RequestsPerSecond <= 0 {
		// Rate limiting is disabled, no validation needed
		return nil
	}
	if c.Burst <= 0 {
		return errors.Newf("rate limit burst must be greater than 0 when rate limiting is enabled")
	}
	if c.Burst > c.RequestsPerSecond {
		return errors.Newf("rate limit burst (%d) must be less than or equal to requests-per-second (%d)",
			c.Burst, c.RequestsPerSecond)
	}
	return nil
}
