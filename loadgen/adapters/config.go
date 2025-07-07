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
		OrdererClient     *OrdererClientConfig     `mapstructure:"orderer-client"`
		SidecarClient     *SidecarClientConfig     `mapstructure:"sidecar-client"`
		CoordinatorClient *CoordinatorClientConfig `mapstructure:"coordinator-client"`
		VCClient          *VCClientConfig          `mapstructure:"vc-client"`
		VerifierClient    *VerifierClientConfig    `mapstructure:"verifier-client"`
		LoadGenClient     *LoadGenClientConfig     `mapstructure:"loadgen-client"`
	}

	// OrdererClientConfig is a struct that contains the configuration for the orderer client.
	OrdererClientConfig struct {
		Orderer              broadcastdeliver.Config `mapstructure:"orderer"`
		BroadcastParallelism int                     `mapstructure:"broadcast-parallelism"`
		// SidecarEndpoint is used to deliver status from the sidecar.
		// If omitted, we will fetch directly from the orderer.
		SidecarEndpoint *connection.Endpoint `mapstructure:"sidecar-endpoint"`
	}

	// SidecarClientConfig is a struct that contains the configuration for the sidecar client.
	SidecarClientConfig struct {
		ChannelID       string                     `mapstructure:"channel-id"`
		SidecarEndpoint *connection.Endpoint       `mapstructure:"sidecar-endpoint"`
		OrdererServers  []*connection.ServerConfig `mapstructure:"orderer-servers"`
	}

	// CoordinatorClientConfig is a struct that contains the configuration for the coordinator client.
	CoordinatorClientConfig struct {
		Endpoint *connection.Endpoint `mapstructure:"endpoint"`
	}

	// VCClientConfig is a struct that contains the configuration for the VC client.
	VCClientConfig struct {
		Endpoints []*connection.Endpoint `mapstructure:"endpoints"`
	}

	// VerifierClientConfig is a struct that contains the configuration for the verifier client.
	VerifierClientConfig struct {
		Endpoints []*connection.Endpoint `mapstructure:"endpoints"`
	}

	// LoadGenClientConfig is a struct that contains the configuration for the load generator client.
	LoadGenClientConfig struct {
		Endpoint *connection.Endpoint `mapstructure:"endpoint"`
	}
)
