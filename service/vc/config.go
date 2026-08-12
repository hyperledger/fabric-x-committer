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
	WorkersForPreparer                int           `mapstructure:"workers-for-preparer" default:"1" validate:"gt=0"`
	WorkersForValidator               int           `mapstructure:"workers-for-validator" default:"1" validate:"gt=0"`
	WorkersForCommitter               int           `mapstructure:"workers-for-committer" default:"20" validate:"gt=0"`
	MaxWorkersForSnapshotHash         int           `mapstructure:"max-workers-for-snapshot-hash" default:"4" validate:"gt=0"`           //nolint:lll,revive
	SnapshotHashBatchSize             int           `mapstructure:"snapshot-hash-batch-size" default:"1000" validate:"gt=0"`             //nolint:lll,revive
	MinTransactionBatchSize           int           `mapstructure:"min-transaction-batch-size" default:"1" validate:"gt=0"`              //nolint:lll,revive
	TimeoutForMinTransactionBatchSize time.Duration `mapstructure:"timeout-for-min-transaction-batch-size" default:"5s" validate:"gt=0"` //nolint:lll,revive
	QueueMultiplier                   int           `mapstructure:"queue-multiplier" default:"1" validate:"gt=0"`
}
