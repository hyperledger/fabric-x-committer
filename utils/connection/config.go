/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"crypto/tls"

	"google.golang.org/grpc/credentials"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

type (
	// MultiClientConfig contains the endpoints, TLS config, and retry profile.
	// This config allows the support of number of different endpoints to multiple service instances.
	MultiClientConfig struct {
		Endpoints   []*Endpoint       `mapstructure:"endpoints"`
		TLS         TLSConfig         `mapstructure:"tls"`
		Retry       *retry.Profile    `mapstructure:"reconnect"`
		FlowControl FlowControlConfig `mapstructure:"flow-control"`
	}

	// ClientConfig contains a single endpoint, TLS config, and retry profile.
	ClientConfig struct {
		Endpoint    *Endpoint         `mapstructure:"endpoint"`
		TLS         TLSConfig         `mapstructure:"tls"`
		Retry       *retry.Profile    `mapstructure:"reconnect"`
		FlowControl FlowControlConfig `mapstructure:"flow-control"`
	}

	// FlowControlConfig sizes a connection's HTTP/2 flow control windows. It is per client and per
	// server because the right size depends on the link's message sizes and round-trip time. Zero
	// keeps gRPC's own BDP-based tuning, which any explicit value disables. The defaults apply only to
	// a config loaded through viper. See docs/performance-tuning.md.
	//
	// A window also bounds the unread data one peer can make a server hold, so limit the client-facing
	// endpoints with MaxConcurrentStreams and RateLimit rather than by shrinking it.
	FlowControlConfig struct {
		// InitialWindowSize is the per-stream window in bytes.
		InitialWindowSize int32 `mapstructure:"initial-window-size" default:"16777216" validate:"gte=0"`
		// InitialConnWindowSize is the connection-level window in bytes, shared by the streams on a
		// connection, so it should exceed InitialWindowSize.
		InitialConnWindowSize int32 `mapstructure:"initial-conn-window-size" default:"33554432" validate:"gte=0"`
	}

	// TLSConfig holds the TLS options and certificate paths
	// used for secure communication between servers and clients.
	// Credentials are built based on the configuration mode.
	// For example, If only server-side TLS is required, the certificate pool (certPool) is not built (for a server),
	// since the relevant certificates paths are defined in the YAML according to the selected mode.
	TLSConfig struct {
		Mode string `mapstructure:"mode" validate:"omitempty,oneof=tls mtls none"`
		// CertPath is the path to the certificate file (public key).
		CertPath string `mapstructure:"cert-path"`
		// KeyPath is the path to the key file (private key).
		KeyPath     string   `mapstructure:"key-path"`
		CACertPaths []string `mapstructure:"ca-cert-paths"`
	}
)

// usage: TLS configuration modes.
const (
	UnmentionedTLSMode = ""
	NoneTLSMode        = "none"
	OneSideTLSMode     = "tls"
	MutualTLSMode      = "mtls"
	DefaultTLSMode     = NoneTLSMode

	// DefaultTLSMinVersion is the minimum version required to achieve secure connections.
	DefaultTLSMinVersion = tls.VersionTLS12
)

// ClientCredentials converts TLSConfig into a TLSCredentials struct and generates client creds.
func (c TLSConfig) ClientCredentials() (credentials.TransportCredentials, error) {
	tlsCreds, err := NewClientTLSCredentials(c)
	if err != nil {
		return nil, err
	}
	return NewClientGRPCTransportCredentials(tlsCreds)
}

// ServerCredentials converts TLSConfig into a TLSCredentials struct and generates server creds.
func (c TLSConfig) ServerCredentials() (credentials.TransportCredentials, error) {
	tlsCreds, err := NewServerTLSCredentials(c)
	if err != nil {
		return nil, err
	}
	return NewServerGRPCTransportCredentials(tlsCreds)
}
