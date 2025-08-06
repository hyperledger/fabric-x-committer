/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cockroachdb/errors"
	"github.com/jackc/puddle"
	"github.com/yugabyte/pgx/v4/pgxpool"
	"go.uber.org/zap"
)

// RetryProfile can be used to define the backoff properties for retries.
//
// We use it as a workaround for a known issues:
//   - Dropping a database with proximity to accessing it.
//     See: https://support.yugabyte.com/hc/en-us/articles/10552861830541-Unable-to-Drop-Database.
//   - Creating/dropping tables immediately after creating a database.
//     See: https://github.com/yugabyte/yugabyte-db/issues/14519.
type RetryProfile struct {
	InitialInterval     time.Duration `mapstructure:"initial-interval" yaml:"initial-interval"`
	RandomizationFactor float64       `mapstructure:"randomization-factor" yaml:"randomization-factor"`
	Multiplier          float64       `mapstructure:"multiplier" yaml:"multiplier"`
	MaxInterval         time.Duration `mapstructure:"max-interval" yaml:"max-interval"`
	// After MaxElapsedTime the ExponentialBackOff returns RetryStopDuration.
	// It never stops if MaxElapsedTime == 0.
	MaxElapsedTime time.Duration `mapstructure:"max-elapsed-time" yaml:"max-elapsed-time"`
	// LBPolicy selects the gRPC load balancing policy: "pick_first" (default) or "round_robin".
	LBPolicy string `mapstructure:"lb-policy" yaml:"lb-policy"`
}

const (
	defaultInitialInterval     = 500 * time.Millisecond
	defaultRandomizationFactor = 0.5
	defaultMultiplier          = 1.5
	defaultMaxInterval         = 10 * time.Second
	defaultMaxElapsedTime      = 15 * time.Minute

	// LBPolicyPickFirst and LBPolicyRoundRobin set the load balancing policy for the grpc client connection.
	LBPolicyPickFirst  = "pick_first"
	LBPolicyRoundRobin = "round_robin" //nolint:revive
)

// Execute executes the given operation repeatedly until it succeeds or a timeout occurs.
// It returns nil on success, or the error returned by the final attempt on timeout.
func (p *RetryProfile) Execute(ctx context.Context, o backoff.Operation) error {
	return backoff.Retry(o, backoff.WithContext(p.NewBackoff(), ctx))
}

// ExecuteSQL executes the given SQL statement until it succeeds or a timeout occurs.
func (p *RetryProfile) ExecuteSQL(ctx context.Context, executor *pgxpool.Pool, sqlStmt string, args ...any) error {
	err := p.Execute(ctx, func() error {
		_, err := executor.Exec(ctx, sqlStmt, args...)
		wrappedErr := errors.Wrapf(err, "failed to execute the SQL statement [%s]", sqlStmt)
		if errors.Is(err, puddle.ErrClosedPool) {
			return &backoff.PermanentError{Err: wrappedErr}
		}
		if wrappedErr != nil {
			logger.WithOptions(zap.AddCallerSkip(8)).Warn(wrappedErr)
		}
		return wrappedErr
	})
	return err
}

// NewBackoff creates a new [backoff.ExponentialBackOff] instance with this profile.
func (p *RetryProfile) NewBackoff() *backoff.ExponentialBackOff {
	b := &backoff.ExponentialBackOff{
		InitialInterval:     defaultInitialInterval,
		RandomizationFactor: defaultRandomizationFactor,
		Multiplier:          defaultMultiplier,
		MaxInterval:         defaultMaxInterval,
		MaxElapsedTime:      defaultMaxElapsedTime,
		Clock:               backoff.SystemClock,
	}
	if p != nil {
		if p.InitialInterval > 0 {
			b.InitialInterval = p.InitialInterval
		}
		if p.RandomizationFactor > 0 {
			b.RandomizationFactor = p.RandomizationFactor
		}
		if p.Multiplier > 1 {
			b.Multiplier = p.Multiplier
		}
		if p.MaxInterval > 0 {
			b.MaxInterval = p.MaxInterval
		}
		if p.MaxElapsedTime > 0 {
			b.MaxElapsedTime = p.MaxElapsedTime
		}
	}
	b.Stop = backoff.Stop // -1 to stop retries
	b.Reset()
	return b
}

// MakeGrpcRetryPolicyJSON defines the retry policy for a gRPC client connection.
// The retry policy applies to all subsequent gRPC calls made through the client connection.
// Our GRPC retry policy is applicable only for the following status codes:
//
//	(1) UNAVAILABLE	The service is currently unavailable (e.g., transient network issue, server down).
//	(2) DEADLINE_EXCEEDED	Operation took too long (deadline passed).
//	(3) RESOURCE_EXHAUSTED	Some resource (e.g., quota) has been exhausted; the operation cannot proceed.
func (p *RetryProfile) MakeGrpcRetryPolicyJSON() string {
	// We initialize a backoff object to fetch the default values.
	b := p.NewBackoff()

	// We put limits on the values to ensure correct values.
	initialInterval := max(b.InitialInterval.Seconds(), time.Nanosecond.Seconds())
	maxInterval := max(b.MaxInterval.Seconds(), initialInterval)
	multiplier := max(b.Multiplier, 1.0001)
	maxElapsedTime := max(b.MaxElapsedTime.Seconds(), maxInterval)

	// determine LB policy from the profile (default to pick_first).
	lbPolicy := normalizeLBPolicy(p)
	logger.Infof("using %v grpc client load balancing policy", lbPolicy)

	ret := map[string]any{
		"loadBalancingConfig": []map[string]any{{
			lbPolicy: make(map[string]any),
		}},
		"methodConfig": []map[string]any{{
			// Setting an empty name sets the default for all methods.
			"name": []any{make(map[string]any)},
			"retryPolicy": map[string]any{
				"maxAttempts":       calcMaxAttempts(initialInterval, maxInterval, multiplier, maxElapsedTime),
				"initialBackoff":    formatSeconds(initialInterval),
				"maxBackoff":        formatSeconds(maxInterval),
				"backoffMultiplier": multiplier,
				"retryableStatusCodes": []string{
					"UNAVAILABLE",
					"DEADLINE_EXCEEDED",
					"RESOURCE_EXHAUSTED",
				},
			},
		}},
	}
	jsonString, err := json.MarshalIndent(ret, "", "  ")
	if err != nil {
		logger.Warnf("failed to marshal retry profile to JSON: %s", err)
		return "{}"
	}
	return string(jsonString)
}

func formatSeconds(sec float64) string {
	return fmt.Sprintf("%ss", strconv.FormatFloat(sec, 'f', -1, 64))
}

// calcMaxAttempts calculates the number of attempts given the following parameters:
// - initialInterval > 0
// - maxInterval     >= i
// - multiplier      > 1
// - maxElapsedTime > i.
func calcMaxAttempts(initialInterval, maxInterval, multiplier, maxElapsedTime float64) int {
	nextBackoffInterval := initialInterval
	var estimatedElapsedTime float64
	var attempts int
	for attempts = 0; estimatedElapsedTime <= maxElapsedTime; attempts++ {
		estimatedElapsedTime += nextBackoffInterval
		nextBackoffInterval = min(nextBackoffInterval*multiplier, maxInterval)
	}
	return attempts
}

func normalizeLBPolicy(p *RetryProfile) string {
	if p == nil {
		return LBPolicyPickFirst
	}
	switch p.LBPolicy {
	case "", LBPolicyPickFirst:
		return LBPolicyPickFirst
	case LBPolicyRoundRobin:
		return LBPolicyRoundRobin
	default:
		logger.Warnf("unknown load balancing policy %q; defaulting to pick_first", p.LBPolicy)
		return LBPolicyPickFirst
	}
}
