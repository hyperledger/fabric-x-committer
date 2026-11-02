/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"time"
)

type (
	// Config describes the load generator metrics.
	// It adds latency tracker to the common metrics configurations.
	Config struct {
		Latency LatencyConfig `mapstructure:"latency" yaml:"latency"`
	}

	// LatencyConfig describes the latency monitoring parameters.
	// MaxTrackedTXs bounds the table of in-flight sampled TXs, and so how long a sampled TX can
	// take to come back before its slot is reused and the measurement lost. Sized against the
	// sampling rate times the expected round trip.
	LatencyConfig struct {
		MaxTrackedTXs uint64        `mapstructure:"max-tracked-txs" default:"10000"`
		SamplerConfig SamplerConfig `mapstructure:"sampler"`
		BucketConfig  BucketConfig  `mapstructure:"buckets"`
	}

	// SamplerConfig describes the latency sampling parameters.
	// Prefix checks for TXs that have the given prefix.
	// Portion uses the simple and efficient hash of the key to sample the required portion of TXs.
	SamplerConfig struct {
		Prefix  string  `mapstructure:"prefix" json:"prefix,omitempty"`
		Portion float64 `mapstructure:"portion" json:"portion,omitempty"`
	}

	// BucketConfig describes the latency bucket distribution.
	BucketConfig struct {
		Distribution BucketDistribution `mapstructure:"distribution"`
		MaxLatency   time.Duration      `mapstructure:"max-latency"`
		BucketCount  int                `mapstructure:"bucket-count"`
		Values       []float64          `mapstructure:"values"`
	}

	// BucketDistribution can be empty, uniform, or fixed.
	BucketDistribution string
	// TraceSamplerType can be always, never, prefix, hash, or timer.
	TraceSamplerType string

	// KeyTracingSampler returns true to sample a given key.
	KeyTracingSampler = func(key string) bool
	// NumberTracingSampler returns true to sample a given number.
	NumberTracingSampler = func(blockNumber uint64) bool
)

// Sample and bucket constants.
const (
	BucketEmpty   BucketDistribution = "empty"
	BucketUniform BucketDistribution = "uniform"
	BucketFixed   BucketDistribution = "fixed"
)
