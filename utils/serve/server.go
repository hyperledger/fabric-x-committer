/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"context"
	"regexp"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/cockroachdb/errors"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

var (
	// listenRetry is the acceptable retry profile if port conflicts occur.
	// This handles the race condition where another process claims a pre-assigned port.
	// This will retry with the same port, waiting for it to become available.
	listenRetry = retry.Profile{
		InitialInterval: 50 * time.Millisecond,
		MaxInterval:     500 * time.Millisecond,
		MaxElapsedTime:  2 * time.Minute,
	}

	// portConflictRegex is the compiled regular expression
	// to efficiently detect port binding conflict errors.
	portConflictRegex = regexp.MustCompile(`(?i)(address\s+already\s+in\s+use|port\s+is\s+already\s+allocated)`)
)

// ListenRetryExecute executes the provided function with retry logic for port binding conflicts.
// It automatically retries when port conflicts are detected (e.g., "address already in use"),
// using exponential backoff. Non-port-conflict errors are treated as permanent failures
// and will not be retried. The retry behavior is controlled by the listenRetry profile.
func ListenRetryExecute(ctx context.Context, f func() error) error {
	return retry.Execute(ctx, &listenRetry, func() error {
		err := f()
		switch {
		case err == nil:
			return nil
		case portConflictRegex.MatchString(err.Error()):
			// Port conflict - will retry with backoff.
			return errors.Wrap(err, "port conflict")
		default:
			// Not a port conflict - return permanent error to stop retrying.
			return backoff.Permanent(errors.Wrap(err, "creating listener"))
		}
	})
}

// DefaultHealthCheckService returns a health-check service that returns SERVING for all services.
func DefaultHealthCheckService() *health.Server {
	healthcheck := health.NewServer()
	healthcheck.SetServingStatus("", healthgrpc.HealthCheckResponse_SERVING)
	return healthcheck
}
