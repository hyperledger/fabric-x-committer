package cliutil

import (
	"github.com/cockroachdb/errors"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// NewDynamicTLS returns the TLS interfaces separately to avoid the Go
// nil-interface trap: a nil *DynamicTLS assigned to an interface becomes a
// non-nil interface wrapping a nil pointer, which passes != nil checks but
// panics on method calls. Returning interfaces directly ensures that when
// TLS is disabled, callers receive true nil values.
func NewDynamicTLS(
	serverConfig *connection.ServerConfig,
) (connection.TLSCertUpdater, connection.TLSConfigProvider, error) {
	if serverConfig == nil || serverConfig.TLS.Mode != connection.MutualTLSMode {
		return nil, nil, nil
	}

	dynamicTLS, err := connection.NewDynamicTLSFromConfig(serverConfig.TLS)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create dynamic TLS config")
	}

	return dynamicTLS, dynamicTLS, nil
}
