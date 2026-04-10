/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/periodic"
)

// Manager is the main component of the dependency graph module.
// It is responsible for managing the local dependency constructor
// and the global dependency manager.
type Manager struct {
	localDepConstructor         *localDependencyConstructor
	globalDepManager            *globalDependencyManager
	parameters                  *Parameters
	outgoingTxsNodeWithLocalDep chan *transactionNodeBatch
	metrics                     *perfMetrics
}

// NewManager creates a new dependency graph manager.
func NewManager(p *Parameters) *Manager {
	metrics := newPerformanceMetrics(p.PrometheusMetricsProvider)

	outgoingTxsNodeWithLocalDep := make(chan *transactionNodeBatch, cap(p.IncomingTxs))
	ldp := newLocalDependencyConstructor(p.IncomingTxs, outgoingTxsNodeWithLocalDep, metrics)

	gdConf := &globalDepConfig{
		incomingTxsNode:        outgoingTxsNodeWithLocalDep,
		outgoingDepFreeTxsNode: p.OutgoingDepFreeTxsNode,
		validatedTxsNode:       p.IncomingValidatedTxsNode,
		waitingTxsLimit:        p.WaitingTxsLimit,
		metrics:                metrics,
	}

	gdp := newGlobalDependencyManager(gdConf)

	return &Manager{
		localDepConstructor:         ldp,
		globalDepManager:            gdp,
		parameters:                  p,
		outgoingTxsNodeWithLocalDep: outgoingTxsNodeWithLocalDep,
		metrics:                     metrics,
	}
}

// Run starts the dependency graph manager by starting the
// local dependency constructors and global dependency graph manager.
func (m *Manager) Run(ctx context.Context) {
	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return m.monitorQueues(gCtx)
	})

	g.Go(func() error {
		m.localDepConstructor.run(gCtx, m.parameters.NumOfLocalDepConstructors)
		return nil
	})

	g.Go(func() error {
		m.globalDepManager.run(gCtx)
		return nil
	})

	_ = g.Wait()
}

func (m *Manager) monitorQueues(ctx context.Context) error {
	// TODO: make sampling time configurable
	for range periodic.Ticker(ctx, 100*time.Millisecond) {
		if ctx.Err() != nil {
			break
		}
		promutil.SetGauge(m.metrics.ldgInputTxBatchQueueSize, len(m.localDepConstructor.incomingTransactions))
		promutil.SetGauge(m.metrics.gdgInputTxBatchQueueSize, len(m.globalDepManager.incomingTransactionsNode))
	}
	return ctx.Err()
}
