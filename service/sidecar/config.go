/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
)

type (
	// Config holds the configuration of the sidecar service. This includes
	// sidecar endpoint, committer endpoint to which the sidecar pushes the block and pulls statuses,
	// and the config of ledger service, and the orderer setup.
	// It may contain the orderer endpoint from which the sidecar pulls blocks.
	Config struct {
		Committer *connection.ClientConfig `mapstructure:"committer"`
		Orderer   ordererdial.Config       `mapstructure:"orderer"`
		Ledger    LedgerConfig             `mapstructure:"ledger"`
		// LastCommittedBlockSetInterval is the interval at which the sidecar updates
		// the coordinator with the last committed block.
		LastCommittedBlockSetInterval time.Duration `mapstructure:"last-committed-block-set-interval" validate:"required,gt=0"` //nolint:lll,revive
		WaitingTxsLimit               int           `mapstructure:"waiting-txs-limit" validate:"required,gt=0"`
		// ChannelBufferSize is the buffer size that will be used to queue blocks, requests, and statuses.
		ChannelBufferSize int                       `mapstructure:"channel-buffer-size" validate:"required,gt=0"`
		Notification      NotificationServiceConfig `mapstructure:"notification"`
	}

	// LedgerConfig holds the ledger path.
	LedgerConfig struct {
		Path string `mapstructure:"path"`
		// SyncInterval controls how often the block store fsyncs to durable storage.
		// A value of N means every Nth block triggers a full sync; intermediate
		// blocks are written without fsync. A value of 0 or 1 means every block is synced.
		SyncInterval uint64 `mapstructure:"sync-interval"`
	}

	// NotificationServiceConfig holds the parameters for notifications.
	NotificationServiceConfig struct {
		// MaxTimeout is an upper limit on the request's timeout to prevent resource exhaustion.
		// If a request doesn't specify a timeout, this value will be used.
		MaxTimeout time.Duration `mapstructure:"max-timeout" validate:"required,gt=0"`
		// MaxActiveTxIDs is the global limit on total active txID subscriptions across all streams.
		MaxActiveTxIDs int `mapstructure:"max-active-tx-ids" validate:"required,gt=0"`
		// MaxTxIDsPerRequest is the maximum number of txIDs allowed in a single notification request.
		MaxTxIDsPerRequest int `mapstructure:"max-tx-ids-per-request" validate:"required,gt=0"`
	}
)

// Default configuration values for the sidecar service.
const (
	DefaultServerPort                    = 4001
	DefaultMonitoringPort                = 2114
	DefaultNotificationMaxTimeout        = time.Minute
	DefaultBufferSize                    = 100
	DefaultMaxActiveTxIDs                = 100_000
	DefaultMaxTxIDsPerRequest            = 1000
	DefaultLastCommittedBlockSetInterval = 5 * time.Second
	DefaultWaitingTxsLimit               = 100_000
	DefaultMaxConcurrentStreams          = 10
)
