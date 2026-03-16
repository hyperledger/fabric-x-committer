/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"hash/maphash"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

var logger = flogging.MustGetLogger("tracker")

type (
	// latencyReceiverSender is used to track TX E2E latency.
	latencyReceiverSender struct {
		validLatency   prometheus.Histogram
		invalidLatency prometheus.Histogram
		txSampler      KeyTracingSampler
		latencyTracker []atomic.Pointer[trackedTX]
		// hashSeed must be different from the one used in the tx sampler to
		// ensure uniform distribution of TXs in the latencyTracker array.
		hashSeed maphash.Seed
	}

	trackedTX struct {
		id      string
		created time.Time
	}
)

const (
	// portionEps is chosen to ensure 1 > (1-eps).
	portionEps float64 = 1e-16
)

func newLatencyReceiverSender(p *monitoring.Provider, conf *LatencyConfig) *latencyReceiverSender {
	buckets := newBuckets(&conf.BucketConfig)
	txSampler := sampleNothing
	maxTrackedTXs := uint64(0)
	if len(buckets) > 0 {
		txSampler = newSampler(&conf.SamplerConfig)
		maxTrackedTXs = max(1, conf.SamplerConfig.MaxTrackedTXs)
	}
	return &latencyReceiverSender{
		validLatency: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: "loadgen",
			Name:      "valid_transaction_latency_seconds",
			Help:      "Latency of transactions in seconds",
			Buckets:   buckets,
		}),
		invalidLatency: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: "loadgen",
			Name:      "invalid_transaction_latency_seconds",
			Help:      "Latency of invalid transactions in seconds",
			Buckets:   buckets,
		}),
		txSampler:      txSampler,
		latencyTracker: make([]atomic.Pointer[trackedTX], maxTrackedTXs),
		hashSeed:       maphash.MakeSeed(),
	}
}

// onSendTransaction is called when a TX is submitted.
func (c *latencyReceiverSender) onSendTransaction(txID string) {
	if !c.txSampler(txID) {
		return
	}
	idx := maphash.String(c.hashSeed, txID) % uint64(len(c.latencyTracker))
	c.latencyTracker[idx].Store(&trackedTX{id: txID, created: time.Now()})
}

// onReceiveTransaction is called when a TX is received.
//
//nolint:revive // parameter 'success' seems to be a control flag, but it is not.
func (c *latencyReceiverSender) onReceiveTransaction(txID string, success bool) {
	if !c.txSampler(txID) {
		return
	}
	idx := maphash.String(c.hashSeed, txID) % uint64(len(c.latencyTracker))
	tx := c.latencyTracker[idx].Load()
	if tx == nil || tx.id != txID {
		return
	}
	logger.Debugf("Tracked transaction %s returned with status: %v", txID, success)
	duration := time.Since(tx.created).Seconds()
	if success {
		c.validLatency.Observe(duration)
	} else {
		c.invalidLatency.Observe(duration)
	}
}

// newBuckets returns a list of buckets for latency monitoring.
func newBuckets(c *BucketConfig) []float64 {
	switch c.Distribution {
	case BucketUniform:
		required(c.BucketCount > 0, "invalid bucket count")
		required(c.MaxLatency > 0, "invalid max latency")
		step := c.MaxLatency.Seconds() / float64(c.BucketCount-1)
		buckets := make([]float64, c.BucketCount)
		for i := range c.BucketCount {
			buckets[i] = step * float64(i)
		}
		return buckets
	case BucketFixed:
		required(len(c.Values) > 0, "no fixed values provided")
		sort.Float64s(c.Values)
		return c.Values
	case BucketEmpty, "":
		return []float64{}
	default:
		panic("undefined distribution: " + c.Distribution)
	}
}

// newSampler returns a KeyTracingSampler for latency monitoring.
func newSampler(c *SamplerConfig) KeyTracingSampler {
	switch {
	case len(c.Prefix) > 0:
		prefixSize := len(c.Prefix)
		return func(key string) bool {
			return len(key) >= prefixSize && key[:prefixSize] == c.Prefix
		}
	case c.Portion < portionEps:
		return sampleNothing
	case c.Portion > (float64(1) - portionEps):
		return sampleAll
	default:
		seed := maphash.MakeSeed()
		valueLimit := uint64(math.Round(c.Portion * math.MaxUint64))
		return func(key string) bool {
			return maphash.String(seed, key) < valueLimit
		}
	}
}

func sampleAll(string) bool {
	return true
}

func sampleNothing(string) bool {
	return false
}

//nolint:revive // flag-parameter: parameter 'condition' seems to be a control flag, avoid control coupling.
func required(condition bool, msg string) {
	if !condition {
		panic(errors.New(msg))
	}
}
