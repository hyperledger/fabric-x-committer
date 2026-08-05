/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package statedb

import (
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

type (
	// Config is the configuration for database initialization and connection.
	// This configuration is shared across multiple services (VC, Query) and the init-db CLI command.
	Config struct {
		Endpoints            []*connection.Endpoint `mapstructure:"endpoints" default:"localhost:5433"`
		Username             string                 `mapstructure:"username"`
		Password             string                 `mapstructure:"password"`
		Database             string                 `mapstructure:"database" default:"yugabyte"`
		MaxConnections       int32                  `mapstructure:"max-connections" default:"20" validate:"gt=0"`
		MinConnections       int32                  `mapstructure:"min-connections" default:"1" validate:"gte=0"`
		LoadBalance          bool                   `mapstructure:"load-balance"`
		Retry                *retry.Profile         `mapstructure:"retry" default:"max-elapsed-time=10m"`
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
