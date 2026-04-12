/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/dbconn"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

// Config is the configuration for the validator-committer service.
type Config struct {
	Server         *connection.ServerConfig `mapstructure:"server"`
	Database       *DatabaseConfig          `mapstructure:"database" validate:"required"`
	ResourceLimits *ResourceLimitsConfig    `mapstructure:"resource-limits" validate:"required"`
	Monitoring     *connection.ServerConfig `mapstructure:"monitoring"`
}

// DatabaseConfig is the configuration for the database.
type DatabaseConfig struct {
	Endpoints            []*connection.Endpoint   `mapstructure:"endpoints"`
	Username             string                   `mapstructure:"username"`
	Password             string                   `mapstructure:"password"`
	Database             string                   `mapstructure:"database" validate:"required"`
	MaxConnections       int32                    `mapstructure:"max-connections" validate:"required,gt=0"`
	MinConnections       int32                    `mapstructure:"min-connections" validate:"gte=0"`
	LoadBalance          bool                     `mapstructure:"load-balance"`
	Retry                *retry.Profile           `mapstructure:"retry"`
	TLS                  dbconn.DatabaseTLSConfig `mapstructure:"tls"`
	TablePreSplitTablets int                      `mapstructure:"table-pre-split-tablets"`
}

// DataSourceName returns the data source name of the database.
func (d *DatabaseConfig) DataSourceName() (string, error) {
	return dbconn.DataSourceName(dbconn.DataSourceNameParams{
		Username:        d.Username,
		Password:        d.Password,
		Database:        d.Database,
		EndpointsString: d.EndpointsString(),
		LoadBalance:     d.LoadBalance,
		TLS:             d.TLS,
	})
}

// EndpointsString returns the address:port as a string with comma as a separator between endpoints.
func (d *DatabaseConfig) EndpointsString() string {
	return connection.AddressString(d.Endpoints...)
}

// ResourceLimitsConfig is the configuration for the resource limits.
type ResourceLimitsConfig struct {
	MaxWorkersForPreparer             int           `mapstructure:"max-workers-for-preparer" validate:"required,gt=0"`
	MaxWorkersForValidator            int           `mapstructure:"max-workers-for-validator" validate:"required,gt=0"`
	MaxWorkersForCommitter            int           `mapstructure:"max-workers-for-committer" validate:"required,gt=0"`
	MinTransactionBatchSize           int           `mapstructure:"min-transaction-batch-size" validate:"required,gt=0"`
	TimeoutForMinTransactionBatchSize time.Duration `mapstructure:"timeout-for-min-transaction-batch-size" validate:"required,gt=0"` //nolint:lll,revive
}

// Default configuration values for the validator-committer service and database.
const (
	DefaultServerPort                  = 6001
	DefaultMonitoringPort              = 2116
	DefaultDatabaseName                = "yugabyte"
	DefaultDatabaseMaxConnections      = 20
	DefaultDatabaseMinConnections      = 1
	DefaultDatabaseRetryMaxElapsedTime = 10 * time.Minute
	DefaultMaxWorkersForPreparer       = 1
	DefaultMaxWorkersForValidator      = 1
	DefaultMaxWorkersForCommitter      = 20
	DefaultMinTransactionBatchSize     = 1
	DefaultTimeoutForMinBatchSize      = 5 * time.Second
	DefaultDatabaseEndpointHost        = "localhost"
	DefaultDatabaseEndpointPort        = 5433
)
