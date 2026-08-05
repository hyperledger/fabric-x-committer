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
		// Parallelism How many parallel go routines will be launched
		Parallelism int `mapstructure:"parallelism" default:"4" validate:"gt=0"`
		// BatchSizeCutoff The minimum amount of responses we need to collect before emitting a response
		BatchSizeCutoff int `mapstructure:"batch-size-cutoff" default:"50" validate:"gt=0"`
		// BatchTimeCutoff How often we should empty the non-empty buffer
		BatchTimeCutoff time.Duration `mapstructure:"batch-time-cutoff" default:"500ms" validate:"gt=0"`
		// ChannelBufferSize The size of the buffer of the input channels (increase for high fluctuations of load)
		ChannelBufferSize int `mapstructure:"channel-buffer-size" default:"50" validate:"gt=0"`
	}
)
