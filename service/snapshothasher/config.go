/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package snapshothasher

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// Config is the configuration for the snapshot service.
type Config struct {
	Database *statedb.Config `mapstructure:"database" validate:"required"`

	// PollInterval is how often the service re-reads the latest `_snapshot` record
	// looking for work. Hashing therefore begins within one interval of the snapshot
	// committing; this is the price of a scheduler that is driven purely by durable
	// state, with no notification from the validator-committer or the coordinator.
	PollInterval time.Duration `mapstructure:"poll-interval" default:"1m" validate:"gt=0"`

	ResourceLimits *ResourceLimitsConfig `mapstructure:"resource-limits" validate:"required"`
}

// ResourceLimitsConfig bounds the work a single hash job may do against the
// database, so hashing a large clone cannot starve the cluster that is also
// serving live transactions.
type ResourceLimitsConfig struct {
	// MaxWorkersForHash is the number of tables hashed in parallel within one job.
	MaxWorkersForHash int `mapstructure:"max-workers-for-hash" default:"4" validate:"gt=0"`
	// HashBatchSize is the number of rows fetched per round-trip while scanning a
	// table (keyset pagination), which bounds the memory a hashing worker holds.
	HashBatchSize int `mapstructure:"hash-batch-size" default:"1000" validate:"gt=0"`
}
