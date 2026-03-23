/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package retry

import (
	"context"

	"github.com/cenkalti/backoff/v4"
	"github.com/cockroachdb/errors"
	"github.com/jackc/puddle/v2"
	"github.com/yugabyte/pgx/v5/pgconn"
	"go.uber.org/zap"
)

type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Execute executes the given operation repeatedly until it succeeds or a timeout occurs.
// It returns nil on success, or the error returned by the final attempt on timeout.
func Execute(ctx context.Context, p *Profile, o backoff.Operation) error {
	return backoff.Retry(o, backoff.WithContext(p.NewBackoff(), ctx))
}

// ExecuteSQL executes the given SQL statement until it succeeds or a timeout occurs.
//
//nolint:revive // argument-limit: maximum number of arguments per function exceeded; max 4 but got 5.
func ExecuteSQL(ctx context.Context, p *Profile, e executor, sqlStmt string, args ...any) error {
	return Execute(ctx, p, func() error {
		_, err := e.Exec(ctx, sqlStmt, args...)
		wrappedErr := errors.Wrapf(err, "failed to execute the SQL statement [%s]", sqlStmt)
		if errors.Is(err, puddle.ErrClosedPool) {
			return &backoff.PermanentError{Err: wrappedErr}
		}
		if wrappedErr != nil {
			logger.WithOptions(zap.AddCallerSkip(8)).Warn(wrappedErr)
		}
		return wrappedErr
	})
}
