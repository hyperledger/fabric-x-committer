/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
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
		SidecarClient  *connection.ClientConfig `mapstructure:"sidecar-client"`
		OrdererServers []*serve.ServerConfig    `mapstructure:"orderer-servers"`
		// OutBlockCapacity bounds how many blocks the embedded mock orderer holds between the
		// workload submitting them and the sidecar fetching them, and so bounds the transactions
		// in flight. An unbounded buffer absorbs an overloaded committer instead of showing it: the
		// submission rate stays at whatever was asked for, and only the commit rate reveals what
		// the committer drains.
		//
		// It counts blocks, so size it in transactions: the same value holds twenty times fewer
		// transactions at 500 per block than at 10,000. Keep that transaction count above the
		// sidecar's waiting-txs-limit, or this buffer fills first and the sidecar's limit never
		// engages. Keep it well above a few blocks too: the sidecar reads blocks and headers on two
		// streams from this one buffer, and a buffer that wraps before the slower stream reaches a
		// block loses that block. Zero, as in a config built in Go, keeps the mock orderer's default.
		OutBlockCapacity int `mapstructure:"out-block-capacity" default:"1000" validate:"gte=0"`
	}
)
