/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Config is the configuration for coordinator service. It contains configurations for all managers.
	Config struct {
		Verifier           connection.MultiClientConfig `mapstructure:"verifier"`
		ValidatorCommitter connection.MultiClientConfig `mapstructure:"validator-committer"`
		DependencyGraph    *DependencyGraphConfig       `mapstructure:"dependency-graph" validate:"required"`
		// ChannelBufferSizePerGoroutine defines the buffer size per go-routine.
		ChannelBufferSizePerGoroutine int `mapstructure:"per-channel-buffer-size-per-goroutine" validate:"required,gt=0"` //nolint:lll,revive
	}

	// DependencyGraphConfig is the configuration for dependency graph manager. It contains resource limits.
	DependencyGraphConfig struct {
		NumOfLocalDepConstructors int `mapstructure:"num-of-local-dep-constructors" validate:"required,gt=0"`
		WaitingTxsLimit           int `mapstructure:"waiting-txs-limit" validate:"required,gt=0"`
	}
)

// Default configuration values for the coordinator service.
const (
	DefaultServerPort                    = 9001
	DefaultMonitoringPort                = 2119
	DefaultNumOfLocalDepConstructors     = 1
	DefaultWaitingTxsLimit               = 100_000
	DefaultChannelBufferSizePerGoroutine = 10
)
