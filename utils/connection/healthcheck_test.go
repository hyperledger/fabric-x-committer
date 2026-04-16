/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestRunHealthCheck(t *testing.T) {
	t.Parallel()

	t.Run("returns nil when service is SERVING", func(t *testing.T) {
		t.Parallel()
		serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)

		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		t.Cleanup(cancel)
		test.ServeForTest(ctx, t, serverConfig, nil)

		err := connection.RunHealthCheck(ctx, serverConfig.GRPC.Endpoint, serverConfig.GRPC.TLS)
		require.NoError(t, err)
	})

	t.Run("returns error when service is not reachable", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)

		unreachableEndpoint := connection.Endpoint{Host: "localhost", Port: 1}
		err := connection.RunHealthCheck(ctx, unreachableEndpoint, connection.TLSConfig{Mode: "none"})
		require.Error(t, err)
	})
}
