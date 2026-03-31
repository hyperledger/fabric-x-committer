/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"

	"github.com/cockroachdb/errors"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
)

// RunHealthCheck dials the given endpoint and performs a gRPC health check.
// Returns nil if the service reports SERVING, otherwise returns an error.
func RunHealthCheck(ctx context.Context, endpoint Endpoint, tlsConfig TLSConfig) error {
	conn, err := NewSingleConnection(&ClientConfig{
		Endpoint: &endpoint,
		TLS:      tlsConfig,
	})
	if err != nil {
		return errors.Wrap(err, "failed to create gRPC client")
	}
	defer func() { _ = conn.Close() }()

	resp, err := healthgrpc.NewHealthClient(conn).Check(ctx, &healthgrpc.HealthCheckRequest{})
	if err != nil {
		return errors.Wrap(err, "health check failed")
	}

	if resp.Status != healthgrpc.HealthCheckResponse_SERVING {
		return errors.Newf("service is %s", resp.Status)
	}

	return nil
}
