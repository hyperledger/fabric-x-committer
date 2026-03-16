/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Config describes the load generator metrics.
	// It adds latency tracker to the common metrics configurations.
	Config struct {
		connection.ServerConfig `mapstructure:",squash" yaml:",inline"`
		Latency                 LatencyConfig `mapstructure:"latency" yaml:"latency"`
	}

	// LatencyConfig describes the latency monitoring parameters.
	LatencyConfig struct {
		SamplerConfig SamplerConfig `mapstructure:"sampler"`
		BucketConfig  BucketConfig  `mapstructure:"buckets"`
	}

	// SamplerConfig describes the latency sampling parameters.
	// Prefix checks for TXs that have the given prefix.
	// Portion uses the simple and efficient hash of the key to sample the required portion of TXs.
	// MaxTrackedTXs is used to prevent unbounded growth of the tracked TXs data structure.
	SamplerConfig struct {
		Prefix        string  `mapstructure:"prefix" json:"prefix,omitempty"`
		Portion       float64 `mapstructure:"portion" json:"portion,omitempty"`
		MaxTrackedTXs uint64  `mapstructure:"max-tracked-txs" json:"max-tracked-txs,omitempty"`
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
