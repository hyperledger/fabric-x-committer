/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import "github.com/hyperledger/fabric-x-committer/utils/monitoring"

// Parameters holds the configuration for the dependency graph manager.
type Parameters struct {
	// IncomingTxs is the channel for dependency manager to receive
	// incoming transactions.
	IncomingTxs <-chan *TransactionBatch
	// OutgoingDepFreeTxsNode is the channel dependency manager to send
	// dependency free transactions for validation and commit.
	OutgoingDepFreeTxsNode chan<- TxNodeBatch
	// IncomingValidatedTxsNode is the channel for dependency manager
	// to receive validated transactions.
	IncomingValidatedTxsNode <-chan TxNodeBatch
	// NumOfLocalDepConstructors defines the number of local
	// dependency constructors.
	NumOfLocalDepConstructors int
	// WaitingTxsLimit defines the maximum number of transactions
	// that can be waiting at the dependency manager.
	WaitingTxsLimit int
	// PrometheusMetricsProvider is the provider for Prometheus metrics.
	PrometheusMetricsProvider *monitoring.Provider
}
