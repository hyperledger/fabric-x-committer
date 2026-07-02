/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package db

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

type (
	// Config is the configuration for database initialization and connection.
	// This configuration is shared across multiple services (VC, Query) and the init-db CLI command.
	Config struct {
		Endpoints            []*connection.Endpoint `mapstructure:"endpoints"`
		Username             string                 `mapstructure:"username"`
		Password             string                 `mapstructure:"password"`
		Database             string                 `mapstructure:"database" validate:"required"`
		MaxConnections       int32                  `mapstructure:"max-connections" validate:"required,gt=0"`
		MinConnections       int32                  `mapstructure:"min-connections" validate:"gte=0"`
		LoadBalance          bool                   `mapstructure:"load-balance"`
		Retry                *retry.Profile         `mapstructure:"retry"`
		TLS                  TLSConfig              `mapstructure:"tls"`
		TablePreSplitTablets int                    `mapstructure:"table-pre-split-tablets"`
	}

	// TLSConfig holds the database TLS mode and its necessary credentials.
	TLSConfig struct {
		Mode       string `mapstructure:"mode"`
		CACertPath string `mapstructure:"ca-cert-path"`
	}
)

// DataSourceName returns the data source name of the database.
func (d *Config) DataSourceName() (string, error) {
	return DataSourceName(DataSourceNameParams{
		Username:        d.Username,
		Password:        d.Password,
		Database:        d.Database,
		EndpointsString: d.EndpointsString(),
		LoadBalance:     d.LoadBalance,
		TLS:             d.TLS,
	})
}

// EndpointsString returns the address:port as a string with comma as a separator between endpoints.
func (d *Config) EndpointsString() string {
	return connection.AddressString(d.Endpoints...)
}

// Default configuration values for database connections.
const (
	DefaultName                = "yugabyte"
	DefaultEndpointHost        = "localhost"
	DefaultEndpointPort        = 5433
	DefaultMaxConnections      = 20
	DefaultMinConnections      = 1
	DefaultRetryMaxElapsedTime = 10 * time.Minute
)
