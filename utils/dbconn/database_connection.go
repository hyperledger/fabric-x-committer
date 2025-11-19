/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dbconn

import (
	"fmt"

	"github.com/cockroachdb/errors"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// DatabaseTLSConfig holds the database TLS mode and its necessary credentials.
	DatabaseTLSConfig struct {
		Mode       string `mapstructure:"mode"`
		CACertPath string `mapstructure:"ca-cert-path"`
	}

	// DataSourceNameParams defines the parameters required to construct a database connection string.
	DataSourceNameParams struct {
		Username        string
		Password        string
		Database        string
		EndpointsString string
		LoadBalance     bool
		TLS             DatabaseTLSConfig
	}
)

// DataSourceName returns the data source name of the database.
func DataSourceName(d DataSourceNameParams) (string, error) {
	ret := fmt.Sprintf("postgres://%s:%s@%s/%s?",
		d.Username, d.Password, d.EndpointsString, d.Database)

	switch d.TLS.Mode {
	case connection.NoneTLSMode, connection.UnmentionedTLSMode:
		ret += "sslmode=disable"
	case connection.OneSideTLSMode:
		// Enforce full SSL verification:
		// requires an encrypted connection (TLS),
		// and ensures the server hostname matches the certificate.
		ret += fmt.Sprintf("sslmode=verify-full&sslrootcert=%s", d.TLS.CACertPath)
	case connection.MutualTLSMode:
		return "", errors.Newf("unsupportted db tls mode: %s", d.TLS.Mode)
	default:
		return "", errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			d.TLS.Mode, connection.NoneTLSMode, connection.OneSideTLSMode, connection.MutualTLSMode)
	}
	// The load balancing flag is only available when the server supports it (having multiple nodes).
	// Thus, we only add it when explicitly required. Otherwise, an error will occur.
	if d.LoadBalance {
		ret += "&load_balance=true"
	}
	return ret, nil
}
