/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"time"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils"
)

var logger = flogging.MustGetLogger("tracker")

// latencyReceiverSender is used to track TX E2E latency.
type latencyReceiverSender struct {
	latencyTracker utils.SyncMap[string, time.Time]
	validLatency   prometheus.Histogram
	invalidLatency prometheus.Histogram
	txSampler      KeyTracingSampler
}

// onSendTransaction is called when a TX is submitted.
func (c *latencyReceiverSender) onSendTransaction(txID string) {
	if c.txSampler(txID) {
		c.latencyTracker.Store(txID, time.Now())
	}
}

// onReceiveTransaction is called when a TX is received.
//
//nolint:revive // parameter 'success' seems to be a control flag, but it is not.
func (c *latencyReceiverSender) onReceiveTransaction(txID string, success bool) {
	if !c.txSampler(txID) {
		return
	}
	start, loaded := c.latencyTracker.LoadAndDelete(txID)
	if !loaded {
		return
	}
	logger.Debugf("Tracked transaction %s returned with status: %v", txID, success)
	duration := time.Since(start).Seconds()
	if success {
		c.validLatency.Observe(duration)
	} else {
		c.invalidLatency.Observe(duration)
	}
}
