/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"github.com/hyperledger/fabric-x-committer/utils/broadcastdeliver"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// AdapterConfig contains all adapters configurations.
	AdapterConfig struct {
		OrdererClient     *OrdererClientConfig          `mapstructure:"orderer-client"`
		SidecarClient     *SidecarClientConfig          `mapstructure:"sidecar-client"`
		CoordinatorClient *connection.ClientConfig      `mapstructure:"coordinator-client"`
		VCClient          *connection.MultiClientConfig `mapstructure:"vc-client"`
		VerifierClient    *connection.MultiClientConfig `mapstructure:"verifier-client"`
		LoadGenClient     *connection.ClientConfig      `mapstructure:"loadgen-client"`
	}

	// OrdererClientConfig is a struct that contains the configuration for the orderer client.
	OrdererClientConfig struct {
		Orderer              broadcastdeliver.Config `mapstructure:"orderer"`
		BroadcastParallelism int                     `mapstructure:"broadcast-parallelism"`
		// SidecarClient is used to deliver status from the sidecar.
		// If omitted, we will fetch directly from the orderer.
		SidecarClient *connection.ClientConfig `mapstructure:"sidecar-client"`
	}

	// SidecarClientConfig is a struct that contains the configuration for the sidecar client.
	SidecarClientConfig struct {
		ChannelID      string                     `mapstructure:"channel-id"`
		Client         *connection.ClientConfig   `mapstructure:"client"`
		OrdererServers []*connection.ServerConfig `mapstructure:"orderer-servers"`
	}
)
