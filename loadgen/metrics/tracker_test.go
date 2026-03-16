/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

//nolint:gocognit // cognitive complexity 24 > 15
func TestLatencyTrackerPrefix(t *testing.T) {
	t.Parallel()

	testSize := 100_000
	keys := makeKeys(testSize)

	for _, conf := range []SamplerConfig{
		{Portion: 0}, {Portion: 1}, {Portion: 0.1}, {Portion: 0.01}, {Prefix: "0"}, {Prefix: "00"},
	} {
		t.Run(fmt.Sprintf("%v", &utils.LazyJSON{O: conf}), func(t *testing.T) {
			t.Parallel()
			p := monitoring.NewProvider()
			l := newLatencyReceiverSender(p, &LatencyConfig{
				BucketConfig: BucketConfig{
					Distribution: BucketUniform,
					MaxLatency:   10 * time.Second,
					BucketCount:  1_000,
				},
				SamplerConfig: SamplerConfig{
					Portion:       conf.Portion,
					Prefix:        conf.Prefix,
					MaxTrackedTXs: 10_000,
				},
			})

			var sampleSize int
			wg := sync.WaitGroup{}
			for i, key := range keys {
				isSampled := l.txSampler(key)
				if isSampled {
					sampleSize++
				}
				wg.Go(func() {
					l.onSendTransaction(key)
					if isSampled {
						time.Sleep(time.Duration((rand.Float64()*4 + 1) * float64(time.Second)))
					}
					l.onReceiveTransaction(key, i%2 == 0)
				})
			}
			wg.Wait()

			eps := float64(testSize) * 1e-2
			switch {
			case conf.Prefix != "":
				require.InDelta(t, float64(testSize)/math.Pow(10, float64(len(conf.Prefix))), sampleSize, eps)
			case conf.Portion > 1e-8:
				require.InDelta(t, float64(testSize)*conf.Portion, sampleSize, eps)
			default:
				require.Equal(t, 0, sampleSize)
			}

			actualValidLatency := test.GetMetricValue(t, l.validLatency)
			actualInvalidLatency := test.GetMetricValue(t, l.invalidLatency)
			expectedLatency := math.NaN()
			if sampleSize > 0 {
				expectedLatency = 3
			}
			require.InDelta(t, expectedLatency, actualValidLatency, 0.2)
			require.InDelta(t, expectedLatency, actualInvalidLatency, 0.2)
		})
	}
}

func BenchmarkLatencyTrackerPortion(b *testing.B) {
	for _, conf := range []SamplerConfig{
		{ /* never */ }, {Portion: 1}, {Portion: 0.1}, {Portion: 0.01}, {Prefix: "0"}, {Prefix: "00"},
	} {
		sampler := newSampler(&conf)
		b.Run(fmt.Sprintf("%v", &utils.LazyJSON{O: conf}), func(b *testing.B) {
			txIDs := makeKeys(b.N)
			b.ResetTimer()
			for _, txID := range txIDs {
				sampler(txID)
			}
		})
	}
}

// makeKeys generates 8 bytes keys.
// It reverses the string for the benefit of the prefix sampler.
func makeKeys(count int) []string {
	keys := make([]string, count)
	for i := range keys {
		k := []rune(fmt.Sprintf("%08d", i))
		sz := len(k)
		for j := range sz / 2 {
			k[j], k[sz-j-1] = k[sz-j-1], k[j]
		}
		keys[i] = string(k)
	}
	// We need to shuffle them to avoid correlation between prefix and success status.
	rand.Shuffle(len(keys), func(i, j int) {
		keys[i], keys[j] = keys[j], keys[i]
	})
	return keys
}

func TestBucketConfig(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		config   BucketConfig
		expected []float64
	}{
		{
			name: "uniform distribution with 5 buckets",
			config: BucketConfig{
				Distribution: BucketUniform,
				MaxLatency:   10 * time.Second,
				BucketCount:  5,
			},
			expected: []float64{0, 2.5, 5, 7.5, 10},
		},
		{
			name: "uniform distribution with 2 buckets",
			config: BucketConfig{
				Distribution: BucketUniform,
				MaxLatency:   10 * time.Second,
				BucketCount:  2,
			},
			expected: []float64{0, 10},
		},
		{
			name: "uniform distribution with 3 buckets",
			config: BucketConfig{
				Distribution: BucketUniform,
				MaxLatency:   time.Minute,
				BucketCount:  3,
			},
			expected: []float64{0, 30, 60},
		},
		{
			name: "fixed distribution with sorted values",
			config: BucketConfig{
				Distribution: BucketFixed,
				Values:       []float64{0.1, 0.5, 1.0, 5.0, 10.0},
			},
			expected: []float64{0.1, 0.5, 1.0, 5.0, 10.0},
		},
		{
			name: "fixed distribution with unsorted values",
			config: BucketConfig{
				Distribution: BucketFixed,
				Values:       []float64{10.0, 0.5, 5.0, 0.1, 1.0},
			},
			expected: []float64{0.1, 0.5, 1.0, 5.0, 10.0},
		},
		{
			name: "fixed distribution with single value",
			config: BucketConfig{
				Distribution: BucketFixed,
				Values:       []float64{1.5},
			},
			expected: []float64{1.5},
		},
		{
			name: "empty distribution",
			config: BucketConfig{
				Distribution: BucketEmpty,
			},
			expected: []float64{},
		},
		{
			name: "empty string distribution",
			config: BucketConfig{
				Distribution: "",
			},
			expected: []float64{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.expected, newBuckets(&tc.config))
		})
	}

	for _, tc := range []struct {
		name          string
		config        BucketConfig
		expectedPanic string
	}{
		{
			name: "uniform distribution with zero bucket count",
			config: BucketConfig{
				Distribution: BucketUniform,
				MaxLatency:   10 * time.Second,
				BucketCount:  0,
			},
			expectedPanic: "invalid bucket count",
		},
		{
			name: "uniform distribution with negative bucket count",
			config: BucketConfig{
				Distribution: BucketUniform,
				MaxLatency:   10 * time.Second,
				BucketCount:  -5,
			},
			expectedPanic: "invalid bucket count",
		},
		{
			name: "uniform distribution with zero max latency",
			config: BucketConfig{
				Distribution: BucketUniform,
				MaxLatency:   0,
				BucketCount:  5,
			},
			expectedPanic: "invalid max latency",
		},
		{
			name: "uniform distribution with negative max latency",
			config: BucketConfig{
				Distribution: BucketUniform,
				MaxLatency:   -10 * time.Second,
				BucketCount:  5,
			},
			expectedPanic: "invalid max latency",
		},
		{
			name: "fixed distribution with no values",
			config: BucketConfig{
				Distribution: BucketFixed,
				Values:       []float64{},
			},
			expectedPanic: "no fixed values provided",
		},
		{
			name: "fixed distribution with nil values",
			config: BucketConfig{
				Distribution: BucketFixed,
				Values:       nil,
			},
			expectedPanic: "no fixed values provided",
		},
		{
			name: "undefined distribution",
			config: BucketConfig{
				Distribution: "invalid",
			},
			expectedPanic: "undefined distribution: invalid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Panics(t, func() {
				newBuckets(&tc.config)
			})
		})
	}
}
