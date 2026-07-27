/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// Config is the configuration for coordinator service. It contains configurations for all managers.
type Config struct {
	Verifier           connection.MultiClientConfig `mapstructure:"verifier"`
	ValidatorCommitter connection.MultiClientConfig `mapstructure:"validator-committer"`
	DependencyGraph    *DependencyGraphConfig       `mapstructure:"dependency-graph" validate:"required"`
	// ChannelBufferSizePerGoroutine defines the buffer size per go-routine.
	ChannelBufferSizePerGoroutine int `mapstructure:"per-channel-buffer-size-per-goroutine" default:"10" validate:"gt=0"` //nolint:lll,revive
	// QueueMonitorSamplingTime defines the sampling interval for monitoring queue sizes.
	QueueMonitorSamplingTime time.Duration `mapstructure:"queue-monitor-sampling-time" default:"100ms" validate:"gt=0"`
}

// DependencyGraphConfig is the configuration for dependency graph manager. It contains resource limits.
type DependencyGraphConfig struct {
	NumOfLocalDepConstructors int `mapstructure:"num-of-local-dep-constructors" default:"1" validate:"gt=0"`
	WaitingTxsLimit           int `mapstructure:"waiting-txs-limit" default:"100000" validate:"gt=0"`
	// ChunkSize defines the maximum number of transactions to process in a single chunk for the dependency graph.
	ChunkSize int `mapstructure:"chunk-size" default:"500" validate:"gt=0"`
}
