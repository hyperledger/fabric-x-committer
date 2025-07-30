/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

type (
	// Config is the configuration for coordinator service. It contains configurations for all managers.
	Config struct {
		Server                        *connection.ServerConfig `mapstructure:"server"`
		VerifierConfig                connection.ClientConfig  `mapstructure:"verifier"`
		ValidatorCommitterConfig      connection.ClientConfig  `mapstructure:"validator-committer"`
		DependencyGraphConfig         *DependencyGraphConfig   `mapstructure:"dependency-graph"`
		Monitoring                    monitoring.Config        `mapstructure:"monitoring"`
		ChannelBufferSizePerGoroutine int                      `mapstructure:"per-channel-buffer-size-per-goroutine"`
	}

	// DependencyGraphConfig is the configuration for dependency graph manager. It contains resource limits.
	DependencyGraphConfig struct {
		NumOfLocalDepConstructors int `mapstructure:"num-of-local-dep-constructors"`
		WaitingTxsLimit           int `mapstructure:"waiting-txs-limit"`
	}
)
