/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
)

type (
	// AdapterConfig contains all adapters configurations.
	AdapterConfig struct {
		OrdererClient     *OrdererClientConfig          `mapstructure:"orderer-client"`
		SidecarClient     *SidecarClientConfig          `mapstructure:"sidecar-client"`
		LoadGenClient     *connection.ClientConfig      `mapstructure:"loadgen-client"`
		CoordinatorClient *connection.ClientConfig      `mapstructure:"coordinator-client"`
		VCClient          *connection.MultiClientConfig `mapstructure:"vc-client"`
		VerifierClient    *connection.MultiClientConfig `mapstructure:"verifier-client"`
	}

	// OrdererClientConfig is a struct that contains the configuration for the orderer client.
	OrdererClientConfig struct {
		Orderer              ordererdial.Config `mapstructure:"orderer"`
		BroadcastParallelism int                `mapstructure:"broadcast-parallelism"`
		// SidecarClient is used to deliver status from the sidecar.
		// If omitted, we will fetch directly from the orderer.
		SidecarClient *connection.ClientConfig `mapstructure:"sidecar-client"`
	}

	// SidecarClientConfig is a struct that contains the configuration for the sidecar client.
	// OrdererServers config must correlate with the orderer endpoints of the policy.
	SidecarClientConfig struct {
		SidecarClient  *connection.ClientConfig   `mapstructure:"sidecar-client"`
		OrdererServers []*connection.ServerConfig `mapstructure:"orderer-servers"`
	}
)
