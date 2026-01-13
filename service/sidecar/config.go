/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type (
	// Config holds the configuration of the sidecar service. This includes
	// sidecar endpoint, committer endpoint to which the sidecar pushes the block and pulls statuses,
	// and the config of ledger service, and the orderer setup.
	// It may contain the orderer endpoint from which the sidecar pulls blocks.
	Config struct {
		Server                        *connection.ServerConfig  `mapstructure:"server"`
		Monitoring                    monitoring.Config         `mapstructure:"monitoring"`
		Committer                     *connection.ClientConfig  `mapstructure:"committer"`
		Orderer                       ordererconn.Config        `mapstructure:"orderer"`
		Ledger                        LedgerConfig              `mapstructure:"ledger"`
		Notification                  NotificationServiceConfig `mapstructure:"notification"`
		LastCommittedBlockSetInterval time.Duration             `mapstructure:"last-committed-block-set-interval"`
		WaitingTxsLimit               int                       `mapstructure:"waiting-txs-limit"`
		// ChannelBufferSize is the buffer size that will be used to queue blocks, requests, and statuses.
		ChannelBufferSize int       `mapstructure:"channel-buffer-size"`
		Bootstrap         Bootstrap `mapstructure:"bootstrap"`
	}
	// Bootstrap configures how to obtain the bootstrap configuration.
	Bootstrap struct {
		// GenesisBlockFilePath is the path for the genesis block.
		// If omitted, the local configuration will be used.
		GenesisBlockFilePath string `mapstructure:"genesis-block-file-path" yaml:"genesis-block-file-path,omitempty"`
	}

	// LedgerConfig holds the ledger path.
	LedgerConfig struct {
		Path string `mapstructure:"path"`
	}

	// NotificationServiceConfig holds the parameters for notifications.
	NotificationServiceConfig struct {
		// MaxTimeout is an upper limit on the request's timeout to prevent resource exhaustion.
		// If a request doesn't specify a timeout, this value will be used.
		MaxTimeout time.Duration `mapstructure:"max-timeout"`
	}
)

const (
	defaultNotificationMaxTimeout = time.Minute
	defaultBufferSize             = 100
)
