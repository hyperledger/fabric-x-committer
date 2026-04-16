/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"net"
	"time"

	"github.com/cockroachdb/errors"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Config holds serving infrastructure configuration.
	// This is parsed separately from service-specific configuration.
	// The GRPC serves the GRPC service.
	// The HTTP serves the service's HTTP API.
	// The Monitoring serves the Prometheus metrics service.
	Config struct {
		GRPC       ServerConfig `mapstructure:"server"`
		HTTP       ServerConfig `mapstructure:"http-server"`
		Monitoring ServerConfig `mapstructure:"monitoring"`
	}

	// ServerConfig describes the connection parameter for a server.
	ServerConfig struct {
		Endpoint             connection.Endpoint    `mapstructure:"endpoint"`
		TLS                  connection.TLSConfig   `mapstructure:"tls"`
		KeepAlive            *ServerKeepAliveConfig `mapstructure:"keep-alive"`
		RateLimit            RateLimitConfig        `mapstructure:"rate-limit"`
		MaxConcurrentStreams int                    `mapstructure:"max-concurrent-streams" validate:"gte=0"`
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
)

// Validate checks that the rate limit configuration is valid.
func (c *RateLimitConfig) Validate() error {
	if c == nil || c.RequestsPerSecond == 0 {
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
