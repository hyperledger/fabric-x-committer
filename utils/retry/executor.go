/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package retry

import (
	"context"

	"github.com/cenkalti/backoff/v5"
	"github.com/cockroachdb/errors"
	"github.com/jackc/puddle/v2"
	"github.com/yugabyte/pgx/v5/pgconn"
	"go.uber.org/zap"
)

type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

var errConditionNotSatisfied = errors.New("condition not satisfied")

// ExecuteWithResult executes the given operation repeatedly until it succeeds or a timeout occurs.
// It returns the result of the operation on success, or an error on timeout.
// This is a generic version that can return any type T.
func ExecuteWithResult[T any](ctx context.Context, p *Profile, operation func() (T, error)) (T, error) {
	return executeWithResult(ctx, p, operation)
}

// Execute executes the given operation repeatedly until it succeeds or a timeout occurs.
// It returns nil on success, or the error returned by the final attempt on timeout.
func Execute(ctx context.Context, p *Profile, o func() error) error {
	_, err := executeWithResult(ctx, p, func() (any, error) {
		return nil, o()
	})
	return err
}

// ExecuteSQL executes the given SQL statement until it succeeds or a timeout occurs.
//
//nolint:revive // argument-limit: maximum number of arguments per function exceeded; max 4 but got 5.
func ExecuteSQL(ctx context.Context, p *Profile, e executor, sqlStmt string, args ...any) error {
	_, err := executeWithResult(ctx, p, func() (any, error) {
		_, err := e.Exec(ctx, sqlStmt, args...)
		return nil, errors.Wrapf(err, "failed to execute the SQL statement [%s]", sqlStmt)
	}, puddle.ErrClosedPool)
	return err
}

// WaitForCondition repeatedly evaluates the given condition function until it returns true or a timeout occurs.
// It uses exponential backoff between attempts as defined by the retry profile.
// Returns nil when the condition is satisfied, or an error if the context is cancelled or max elapsed time is reached.
func WaitForCondition(ctx context.Context, p *Profile, condition func() bool) bool {
	res, _ := executeWithResult(ctx, p, func() (bool, error) {
		res := condition()
		if res {
			return res, nil
		}
		return res, errConditionNotSatisfied
	})
	return res
}

// executeWithResult executes the given operation repeatedly until it succeeds or a timeout occurs.
// It returns the result of the operation on success, or an error on timeout.
// This is a generic version that can return any type T.
// We skip 4 callers for logging to always report the calling method.
func executeWithResult[T any](
	ctx context.Context, p *Profile, operation func() (T, error), terminalErrors ...error,
) (T, error) {
	p = p.WithDefaults()
	return backoff.Retry(ctx, func() (T, error) {
		res, err := operation()
		if err != nil {
			logger.WithOptions(zap.AddCallerSkip(4)).Warn(err)
		}
		// We identify common cases where retry isn't useful.
		for _, isErr := range terminalErrors {
			if errors.Is(err, isErr) {
				err = backoff.Permanent(err)
			}
		}
		return res, err
	}, backoff.WithBackOff(p.NewBackoff()), backoff.WithMaxElapsedTime(p.MaxElapsedTime))
}
