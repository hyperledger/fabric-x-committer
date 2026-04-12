/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Package retry provides exponential backoff retry mechanisms for transient failures.
//
// This package wraps github.com/cenkalti/backoff/v5 with application-specific retry patterns
// and error handling semantics. It offers several retry strategies:
//
//   - Execute: Retries an operation until it succeeds or times out
//   - ExecuteWithResult: Generic version that returns a result on success
//   - ExecuteSQL: Specialized retry for SQL operations
//   - WaitForCondition: Polls a condition until it becomes true
//   - Sustain: Keeps a long-running operation alive, restarting on transient failures
//
// The package defines special error types to control retry behavior:
//
//   - ErrNonRetryable: Wraps permanent failures that should stop retries immediately
//   - ErrBackOff: Indicates transient errors that should trigger exponential backoff
//
// Retry behavior is configured through the Profile struct, which defines:
//
//   - InitialInterval: Starting wait time between retries
//   - RandomizationFactor: Jitter to prevent thundering herd
//   - Multiplier: Factor by which wait time increases each retry
//   - MaxInterval: Maximum wait time between retries
//   - MaxElapsedTime: Total time budget for all retries (0 = unlimited)
//
// Example Usage:
//
//	// Simple retry with default profile
//	err := retry.Execute(ctx, nil, func() error {
//	    return someOperation()
//	})
//
//	// Retry with custom profile and result
//	profile := &retry.Profile{
//	    InitialInterval: 100 * time.Millisecond,
//	    MaxElapsedTime:  30 * time.Second,
//	}
//	result, err := retry.ExecuteWithResult(ctx, profile, func() (*Data, error) {
//	    return fetchData()
//	})
//
//	// Keep a service running with automatic restart
//	err := retry.Sustain(ctx, profile, func() error {
//	    return runService() // Returns ErrBackOff on transient errors
//	})
package retry

import (
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
)

var logger = flogging.MustGetLogger("retry")

// Profile can be used to define the backoff properties for retries.
//
// After MaxElapsedTime, the backoff method returns the underlying error.
// It never stops if MaxElapsedTime == 0.
//
// This is used as a workaround for known issues:
//   - Dropping a database with proximity to accessing it.
//     See: https://support.yugabyte.com/hc/en-us/articles/10552861830541-Unable-to-Drop-Database.
//   - Creating/dropping tables immediately after creating a database.
//     See: https://github.com/yugabyte/yugabyte-db/issues/14519.
type Profile struct {
	InitialInterval     time.Duration `mapstructure:"initial-interval"`
	RandomizationFactor float64       `mapstructure:"randomization-factor"`
	Multiplier          float64       `mapstructure:"multiplier"`
	MaxInterval         time.Duration `mapstructure:"max-interval"`
	MaxElapsedTime      time.Duration `mapstructure:"max-elapsed-time"`
}

const (
	defaultInitialInterval     = 500 * time.Millisecond
	defaultRandomizationFactor = 0.5
	defaultMultiplier          = 1.5
	defaultMaxInterval         = 10 * time.Second
	defaultMaxElapsedTime      = 15 * time.Minute
)

// WithDefaults returns a clone of this profile with default values.
func (p *Profile) WithDefaults() *Profile {
	newP := &Profile{
		InitialInterval:     defaultInitialInterval,
		RandomizationFactor: defaultRandomizationFactor,
		Multiplier:          defaultMultiplier,
		MaxInterval:         defaultMaxInterval,
		MaxElapsedTime:      defaultMaxElapsedTime,
	}
	if p == nil {
		return newP
	}
	if p.InitialInterval > 0 {
		newP.InitialInterval = p.InitialInterval
	}
	if p.RandomizationFactor > 0 {
		newP.RandomizationFactor = p.RandomizationFactor
	}
	if p.Multiplier > 1 {
		newP.Multiplier = p.Multiplier
	}
	if p.MaxInterval > 0 {
		newP.MaxInterval = p.MaxInterval
	}
	if p.MaxElapsedTime > 0 {
		newP.MaxElapsedTime = p.MaxElapsedTime
	}
	return newP
}

// NewBackoff creates a new [backoff.ExponentialBackOff] instance with this profile.
//
// Note: MaxElapsedTime is not set on the backoff instance itself. In backoff/v5,
// MaxElapsedTime is passed separately to backoff.Retry() as a configuration option
// via backoff.WithMaxElapsedTime(). This allows the same backoff instance to be
// reused with different timeout policies.
func (p *Profile) NewBackoff() *backoff.ExponentialBackOff {
	pp := p.WithDefaults()
	b := &backoff.ExponentialBackOff{
		InitialInterval:     pp.InitialInterval,
		RandomizationFactor: pp.RandomizationFactor,
		Multiplier:          pp.Multiplier,
		MaxInterval:         pp.MaxInterval,
	}
	b.Reset()
	return b
}
