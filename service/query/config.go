/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package query

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// Config is the configuration for the query service.
// To reduce the applied workload on the database and improve performance we batch views and queries.
// That is, views with the same protoqueryservice.ViewParameters will be batched together
// if they created within the ViewAggregationWindow. But no more than MaxAggregatedViews can be
// batched together.
// Queries from the same view and namespace will be batched together.
// A query batch is ready to be submitted if it has more keys than
// MinBatchKeys, or the batch has waited more than MaxBatchWait.
// Once a batch is ready, it is submitted as soon as there is a connection available,
// up the maximal number of connections defined in the Database configuration.
// Thus, a batch query can wait longer than MaxBatchWait if we don't have available connection.
// To avoid dangling views, a view is limited to a period of MaxViewTimeout.
// MaxViewTimeout includes the time it takes to execute the last query in the view.
// That is, if a query is executed while the timeout is expired, the query will be aborted.
// The number of parallel active views is theoretically unlimited as multiple views can be aggregated
// together. However, the number of active batched views is limited by the maximal
// number of database connections.
// Setting the maximal database connections higher than the following, ensures enough available connections.
// (MaxViewTimeout / ViewAggregationWindow) * <number-of-used-view-configuration-permutations>
// If there are no more available connections, queries will wait until such connection is available.
type Config struct {
	Server                *connection.ServerConfig `mapstructure:"server"`
	Monitoring            *connection.ServerConfig `mapstructure:"monitoring"`
	Database              *vc.DatabaseConfig       `mapstructure:"database" validate:"required"`
	MinBatchKeys          int                      `mapstructure:"min-batch-keys" validate:"required,gt=0"`
	MaxBatchWait          time.Duration            `mapstructure:"max-batch-wait" validate:"required,gt=0"`
	ViewAggregationWindow time.Duration            `mapstructure:"view-aggregation-window" validate:"required,gt=0"`
	MaxAggregatedViews    int                      `mapstructure:"max-aggregated-views" validate:"required,gt=0"`
	MaxActiveViews        int                      `mapstructure:"max-active-views" validate:"gte=0"`
	MaxViewTimeout        time.Duration            `mapstructure:"max-view-timeout" validate:"required,gt=0"`
	// MaxRequestKeys is the maximum number of keys allowed in a single query request.
	// This applies to both GetRows (total keys across all namespaces) and
	// GetTransactionStatus (number of transaction IDs).
	// Set to 0 to disable the limit.
	MaxRequestKeys int `mapstructure:"max-request-keys" validate:"gte=0"`

	// ACLRefreshInterval defines how long the query service caches configuration data
	// before a new connection can trigger a refresh from the database.
	// This prevents excessive database queries when multiple clients connect simultaneously.
	ACLRefreshInterval time.Duration `mapstructure:"acl-refresh-interval" validate:"gte=0"`
	// CaFetchTimeout defines the timeout for fetching CA certificates from the database.
	CAFetchTimeout time.Duration `mapstructure:"ca-fetch-timeout" validate:"gte=0"`
}

// Default configuration values for the query service.
const (
	DefaultServerPort            = 7001
	DefaultMonitoringPort        = 2117
	DefaultRequestsPerSecond     = 5000
	DefaultBurst                 = 1000
	DefaultMinBatchKeys          = 1024
	DefaultMaxBatchWait          = 100 * time.Millisecond
	DefaultViewAggregationWindow = 100 * time.Millisecond
	DefaultMaxAggregatedViews    = 1024
	DefaultMaxActiveViews        = 4096
	DefaultMaxViewTimeout        = 10 * time.Second
	DefaultMaxRequestKeys        = 10000
	DefaultACLRefreshInterval    = 200 * time.Millisecond
	DefaultCAFetchTimeout        = 15 * time.Second
)
