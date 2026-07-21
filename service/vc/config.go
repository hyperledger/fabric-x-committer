/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// Config is the configuration for the validator-committer service.
type Config struct {
	Database       *statedb.Config       `mapstructure:"database" validate:"required"`
	ResourceLimits *ResourceLimitsConfig `mapstructure:"resource-limits" validate:"required"`
}

// ResourceLimitsConfig is the configuration for the resource limits.
type ResourceLimitsConfig struct {
	MaxWorkersForPreparer             int           `mapstructure:"max-workers-for-preparer" validate:"required,gt=0"`
	MaxWorkersForValidator            int           `mapstructure:"max-workers-for-validator" validate:"required,gt=0"`
	MaxWorkersForCommitter            int           `mapstructure:"max-workers-for-committer" validate:"required,gt=0"`
	MinTransactionBatchSize           int           `mapstructure:"min-transaction-batch-size" validate:"required,gt=0"`
	TimeoutForMinTransactionBatchSize time.Duration `mapstructure:"timeout-for-min-transaction-batch-size" validate:"required,gt=0"` //nolint:lll,revive
}

// Default configuration values for the validator-committer service.
const (
	DefaultServerPort              = 6001
	DefaultMonitoringPort          = 2116
	DefaultMaxWorkersForPreparer   = 1
	DefaultMaxWorkersForValidator  = 1
	DefaultMaxWorkersForCommitter  = 20
	DefaultMinTransactionBatchSize = 1
	DefaultTimeoutForMinBatchSize  = 5 * time.Second
)
