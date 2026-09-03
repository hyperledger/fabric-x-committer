/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Manager is the main component of the dependency graph module.
// It is responsible for managing the local dependency constructor
// and the global dependency manager.
type Manager struct {
	localDepConstructor *localDependencyConstructor
	globalDepManager    *globalDependencyManager
	parameters          *Parameters
	metrics             *perfMetrics
}

// NewManager creates a new dependency graph manager.
func NewManager(p *Parameters) *Manager {
	outgoingTxsNodeWithLocalDep := make(chan *transactionNodeBatch, cap(p.IncomingTxs))

	// The queues are reported on scrape, so they must exist before the metrics are registered.
	metrics := newPerformanceMetrics(p.PrometheusMetricsProvider, &managerQueues{
		ldgInput: p.IncomingTxs,
		gdgInput: outgoingTxsNodeWithLocalDep,
	})

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
		localDepConstructor: ldp,
		globalDepManager:    gdp,
		parameters:          p,
		metrics:             metrics,
	}
}

// Run starts the dependency graph manager by starting the
// local dependency constructors and global dependency graph manager.
func (m *Manager) Run(ctx context.Context) {
	g, gCtx := errgroup.WithContext(ctx)

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
