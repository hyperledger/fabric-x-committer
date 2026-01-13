/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"crypto/tls"
	"net"
	"time"

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
		Mode        string   `mapstructure:"mode"`
		CertPath    string   `mapstructure:"cert-path"`
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
	return tlsMaterials.ClientCredentials()
}

// ServerCredentials converts TLSConfig into a TLSMaterials struct and generates server creds.
func (c TLSConfig) ServerCredentials() (credentials.TransportCredentials, error) {
	tlsMaterials, err := NewTLSMaterials(c)
	if err != nil {
		return nil, err
	}
	return tlsMaterials.ServerCredentials()
}
