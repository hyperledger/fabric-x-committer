/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"github.com/spf13/viper"

	"github.com/hyperledger/fabric-x-committer/loadgen"
	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/query"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliverorderer"
)

// NewViperWithCoordinatorDefaults returns a viper instance with the coordinator default values.
func NewViperWithCoordinatorDefaults() *viper.Viper {
	v := newViperWithServiceDefault(coordinator.DefaultServerPort, coordinator.DefaultMonitoringPort)
	v.SetDefault("dependency-graph.num-of-local-dep-constructors", coordinator.DefaultNumOfLocalDepConstructors)
	v.SetDefault("dependency-graph.waiting-txs-limit", coordinator.DefaultWaitingTxsLimit)
	v.SetDefault("per-channel-buffer-size-per-goroutine", coordinator.DefaultChannelBufferSizePerGoroutine)
	return v
}

// NewViperWithSidecarDefaults returns a viper instance with the sidecar default values.
func NewViperWithSidecarDefaults() *viper.Viper {
	v := newViperWithServiceDefault(sidecar.DefaultServerPort, sidecar.DefaultMonitoringPort)
	v.SetDefault("committer.endpoint",
		(&connection.Endpoint{Host: connection.DefaultHost, Port: coordinator.DefaultServerPort}).String())
	v.SetDefault("ledger.path", "./ledger/")
	v.SetDefault("notification.max-timeout", sidecar.DefaultNotificationMaxTimeout)
	v.SetDefault("notification.max-active-tx-ids", sidecar.DefaultMaxActiveTxIDs)
	v.SetDefault("notification.max-tx-ids-per-request", sidecar.DefaultMaxTxIDsPerRequest)
	v.SetDefault("last-committed-block-set-interval", sidecar.DefaultLastCommittedBlockSetInterval)
	v.SetDefault("waiting-txs-limit", sidecar.DefaultWaitingTxsLimit)
	v.SetDefault("channel-buffer-size", sidecar.DefaultBufferSize)
	v.SetDefault("server.max-concurrent-streams", sidecar.DefaultMaxConcurrentStreams)
	v.SetDefault("orderer.suspicion-grace-period-per-block", deliverorderer.DefaultSuspicionGracePeriodPerBlock)
	return v
}

// NewViperWithVerifierDefaults returns a viper instance with the verifier default values.
func NewViperWithVerifierDefaults() *viper.Viper {
	v := newViperWithServiceDefault(verifier.DefaultServerPort, verifier.DefaultMonitoringPort)
	v.SetDefault("parallel-executor.parallelism", verifier.DefaultParallelism)
	v.SetDefault("parallel-executor.batch-time-cutoff", verifier.DefaultBatchTimeCutoff)
	v.SetDefault("parallel-executor.batch-size-cutoff", verifier.DefaultBatchSizeCutoff)
	v.SetDefault("parallel-executor.channel-buffer-size", verifier.DefaultChannelBufferSize)
	return v
}

// NewViperWithVCDefaults returns a viper instance with the VC default values.
func NewViperWithVCDefaults() *viper.Viper {
	v := newViperWithServiceDefault(vc.DefaultServerPort, vc.DefaultMonitoringPort)
	defaultDBFlags(v)
	// defaults for ResourceLimitsConfig
	limitPrefix := "resource-limits."
	v.SetDefault(limitPrefix+"max-workers-for-preparer", vc.DefaultMaxWorkersForPreparer)
	v.SetDefault(limitPrefix+"max-workers-for-validator", vc.DefaultMaxWorkersForValidator)
	v.SetDefault(limitPrefix+"max-workers-for-committer", vc.DefaultMaxWorkersForCommitter)
	v.SetDefault(limitPrefix+"min-transaction-batch-size", vc.DefaultMinTransactionBatchSize)
	v.SetDefault(limitPrefix+"timeout-for-min-transaction-batch-size", vc.DefaultTimeoutForMinBatchSize)
	return v
}

// NewViperWithQueryDefaults returns a viper instance with the query-service default values.
func NewViperWithQueryDefaults() *viper.Viper {
	v := newViperWithServiceDefault(query.DefaultServerPort, query.DefaultMonitoringPort)
	defaultDBFlags(v)
	v.SetDefault("server.rate-limit.requests-per-second", query.DefaultRequestsPerSecond)
	v.SetDefault("server.rate-limit.burst", query.DefaultBurst)
	v.SetDefault("min-batch-keys", query.DefaultMinBatchKeys)
	v.SetDefault("max-batch-wait", query.DefaultMaxBatchWait)
	v.SetDefault("view-aggregation-window", query.DefaultViewAggregationWindow)
	v.SetDefault("max-aggregated-views", query.DefaultMaxAggregatedViews)
	v.SetDefault("max-active-views", query.DefaultMaxActiveViews)
	v.SetDefault("max-view-timeout", query.DefaultMaxViewTimeout)
	v.SetDefault("max-request-keys", query.DefaultMaxRequestKeys)
	v.SetDefault("acl-refresh-interval", query.DefaultACLRefreshInterval)
	v.SetDefault("ca-fetch-timeout", query.DefaultCAFetchTimeout)
	return v
}

// NewViperWithLoadGenDefaults returns a viper instance with the load generator default values.
func NewViperWithLoadGenDefaults() *viper.Viper {
	return newViperWithServiceDefault(loadgen.DefaultServerPort, loadgen.DefaultMonitoringPort)
}

// newViperWithServiceDefault returns a viper instance with a service default values.
func newViperWithServiceDefault(servicePort, monitoringPort int) *viper.Viper {
	v := NewViperWithLoggingDefault()
	v.SetDefault("server.endpoint", &connection.Endpoint{Host: connection.DefaultHost, Port: servicePort})
	v.SetDefault("monitoring.endpoint", &connection.Endpoint{Host: connection.DefaultHost, Port: monitoringPort})
	// Rate limiting disabled by default (0 = disabled)
	v.SetDefault("server.rate-limit", &connection.RateLimitConfig{})
	v.SetDefault("monitoring.rate-limit", &connection.RateLimitConfig{})
	return v
}

// NewViperWithLoggingDefault returns a viper instance with the logging default values.
func NewViperWithLoggingDefault() *viper.Viper {
	v := viper.New()
	v.SetDefault("logging.logSpec", "info")
	v.SetDefault("logging.format", "")
	return v
}

// defaultDBFlags sets the default DB parameters.
func defaultDBFlags(v *viper.Viper) {
	prefix := "database."
	v.SetDefault(prefix+"endpoints", []*connection.Endpoint{
		{Host: vc.DefaultDatabaseEndpointHost, Port: vc.DefaultDatabaseEndpointPort},
	})
	v.SetDefault(prefix+"database", vc.DefaultDatabaseName)
	v.SetDefault(prefix+"max-connections", vc.DefaultDatabaseMaxConnections)
	v.SetDefault(prefix+"min-connections", vc.DefaultDatabaseMinConnections)
	// We allow 10 minutes by default for a DB recovery.
	v.SetDefault(prefix+"retry.max-elapsed-time", vc.DefaultDatabaseRetryMaxElapsedTime)
}
