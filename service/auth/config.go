/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// Config is the configuration for the authentication and authorization service.
type Config struct {
	// Database is the state database the service reads the channel configuration from and persists its
	// token and nonce records in.
	Database *statedb.Config `mapstructure:"database" validate:"required"`
	// TokenTTL is the lifetime of an issued token.
	TokenTTL time.Duration `mapstructure:"token-ttl" default:"5m" validate:"gt=0"`
	// NonceTTL is how long an issued nonce stays redeemable.
	NonceTTL time.Duration `mapstructure:"nonce-ttl" default:"1m" validate:"gt=0"`
	// EnvelopeFreshnessWindow bounds an envelope timestamp's deviation from the server clock.
	EnvelopeFreshnessWindow time.Duration `mapstructure:"envelope-freshness-window" default:"5m" validate:"gt=0"`
	// ConfigRefreshInterval is how often the service reads the latest committed channel configuration
	// from the state database to refresh its evaluation bundle.
	ConfigRefreshInterval time.Duration `mapstructure:"config-refresh-interval" default:"1m" validate:"gt=0"`
	// SweepInterval is how often expired token and nonce records are deleted from the database.
	SweepInterval time.Duration `mapstructure:"sweep-interval" default:"1m" validate:"gt=0"`
	// ChallengeRateLimit caps IssueNonce and Authenticate together: the only RPCs reachable without a token.
	// Limited apart from the server's rate-limit, which Authorize shares with the resource servers' whole load.
	ChallengeRateLimit serve.RateLimitConfig `mapstructure:"challenge-rate-limit" default:"requests-per-second=200 burst=50"` //nolint:lll,revive
}
