/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"time"
)

type (
	// Config describes the signature verifier parameters.
	Config struct {
		ParallelExecutor ExecutorConfig `mapstructure:"parallel-executor"`
	}

	// ExecutorConfig describes the execution parameters.
	ExecutorConfig struct {
		// Parallelism How many parallel go routines will be launched
		Parallelism int `mapstructure:"parallelism" validate:"required,gt=0"`
		// BatchSizeCutoff The minimum amount of responses we need to collect before emitting a response
		BatchSizeCutoff int `mapstructure:"batch-size-cutoff" validate:"required,gt=0"`
		// BatchTimeCutoff How often we should empty the non-empty buffer
		BatchTimeCutoff time.Duration `mapstructure:"batch-time-cutoff" validate:"required,gt=0"`
		// ChannelBufferSize The size of the buffer of the input channels (increase for high fluctuations of load)
		ChannelBufferSize int `mapstructure:"channel-buffer-size" validate:"required,gt=0"`
	}
)

// Default configuration values for the verifier service.
const (
	DefaultServerPort        = 5001
	DefaultMonitoringPort    = 2115
	DefaultParallelism       = 4
	DefaultBatchSizeCutoff   = 50
	DefaultBatchTimeCutoff   = 500 * time.Millisecond
	DefaultChannelBufferSize = 50
)
