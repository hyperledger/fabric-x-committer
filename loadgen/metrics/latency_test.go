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
		conf := conf
		t.Run(fmt.Sprintf("%v", &utils.LazyJSON{O: conf}), func(t *testing.T) {
			t.Parallel()
			p := monitoring.NewProvider()
			l := newLatencyReceiverSender(p, &LatencyConfig{
				BucketConfig: BucketConfig{
					Distribution: BucketUniform,
					MaxLatency:   time.Minute,
					BucketCount:  1_000,
				},
				SamplerConfig: conf,
			})

			var sampleSize int
			wg := sync.WaitGroup{}
			for i := range testSize {
				wg.Add(1)
				key := keys[i]

				isSampled := l.txSampler(key)
				if isSampled {
					sampleSize++
				}
				go func() {
					defer wg.Done()
					l.onSendTransaction(key)
					if isSampled {
						time.Sleep(time.Duration((rand.Float64()*4 + 1) * float64(time.Second)))
					}
					l.onReceiveTransaction(key, true)
				}()
			}
			wg.Wait()

			eps := float64(testSize) * 1e-2
			switch {
			case conf.Prefix != "":
				require.InDelta(t, float64(testSize)/math.Pow(10, float64(len(conf.Prefix))), sampleSize, eps)
			case conf.Portion > 1e-8:
				require.InDelta(t, float64(testSize)*conf.Portion, sampleSize, eps)
			}

			actualLatency := test.GetMetricValue(t, l.validLatency)
			expectedLatency := 0
			if sampleSize > 0 {
				actualLatency /= float64(sampleSize)
				expectedLatency = 3
			}
			require.InDelta(t, expectedLatency, actualLatency, 0.2)
		})
	}
}

func BenchmarkLatencyTrackerPortion(b *testing.B) {
	for _, conf := range []SamplerConfig{
		{ /* never */ }, {Portion: 1}, {Portion: 0.1}, {Portion: 0.01}, {Prefix: "0"}, {Prefix: "00"},
	} {
		sampler := conf.TxSampler()
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
	return keys
}
