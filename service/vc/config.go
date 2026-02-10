/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/dbconn"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

// Config is the configuration for the validator-committer service.
type Config struct {
	Server         *connection.ServerConfig `mapstructure:"server"`
	Database       *DatabaseConfig          `mapstructure:"database"`
	ResourceLimits *ResourceLimitsConfig    `mapstructure:"resource-limits"`
	Monitoring     monitoring.Config        `mapstructure:"monitoring"`
}

// DatabaseConfig is the configuration for the database.
type DatabaseConfig struct {
	Endpoints            []*connection.Endpoint   `mapstructure:"endpoints"`
	Username             string                   `mapstructure:"username"`
	Password             string                   `mapstructure:"password"`
	Database             string                   `mapstructure:"database"`
	MaxConnections       int32                    `mapstructure:"max-connections"`
	MinConnections       int32                    `mapstructure:"min-connections"`
	LoadBalance          bool                     `mapstructure:"load-balance"`
	Retry                *connection.RetryProfile `mapstructure:"retry"`
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
	MaxWorkersForPreparer             int           `mapstructure:"max-workers-for-preparer"`
	MaxWorkersForValidator            int           `mapstructure:"max-workers-for-validator"`
	MaxWorkersForCommitter            int           `mapstructure:"max-workers-for-committer"`
	MinTransactionBatchSize           int           `mapstructure:"min-transaction-batch-size"`
	TimeoutForMinTransactionBatchSize time.Duration `mapstructure:"timeout-for-min-transaction-batch-size"`
}
