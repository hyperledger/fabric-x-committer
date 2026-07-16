/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicemanager

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"

	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestQueueSize(t *testing.T) {
	t.Parallel()
	p := monitoring.NewProvider()
	m := NewMetrics(p, monitoring.MetricsParameters{})
	incomingTasks := make(chan []*dependencygraph.TransactionNode, 10)
	outgoingTasks := make(chan []*dependencygraph.TransactionNode, 10)
	mgr := Manager{
		Params: Parameters{
			ClientConfig:  nil,
			IncomingTasks: incomingTasks,
			OutgoingTasks: outgoingTasks,
			Metrics:       m,
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	go mgr.monitorQueues(ctx)

	incomingTasks <- nil
	outgoingTasks <- nil

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, 1, test.GetIntMetricValue(t, m.InputQueueSize))
		require.Equal(ct, 1, test.GetIntMetricValue(t, m.OutputQueueSize))
	}, 3*time.Second, 500*time.Millisecond)

	<-incomingTasks
	<-outgoingTasks

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Zero(ct, test.GetIntMetricValue(t, m.InputQueueSize))
		require.Zero(ct, test.GetIntMetricValue(t, m.OutputQueueSize))
	}, 3*time.Second, 500*time.Millisecond)
}
