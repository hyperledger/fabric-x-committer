/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Config describes the signature verifier parameters.
	Config struct {
		Server           *connection.ServerConfig `mapstructure:"server" yaml:"server"`
		Monitoring       *connection.ServerConfig `mapstructure:"monitoring" yaml:"monitoring"`
		ParallelExecutor ExecutorConfig           `mapstructure:"parallel-executor" yaml:"parallel-executor"`
	}

	// ExecutorConfig describes the execution parameters.
	ExecutorConfig struct {
		// Parallelism How many parallel go routines will be launched
		Parallelism int `mapstructure:"parallelism" yaml:"parallelism"`
		// BatchSizeCutoff The minimum amount of responses we need to collect before emitting a response
		BatchSizeCutoff int `mapstructure:"batch-size-cutoff" yaml:"batch-size-cutoff"`
		// BatchTimeCutoff How often we should empty the non-empty buffer
		BatchTimeCutoff time.Duration `mapstructure:"batch-time-cutoff" yaml:"batch-time-cutoff"`
		// ChannelBufferSize The size of the buffer of the input channels (increase for high fluctuations of load)
		ChannelBufferSize int `mapstructure:"channel-buffer-size" yaml:"channel-buffer-size"`
	}
)
