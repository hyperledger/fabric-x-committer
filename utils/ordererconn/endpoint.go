/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	commontypes "github.com/hyperledger/fabric-x-common/api/types"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// NewEndpoints is a helper function to generate a list of Endpoint(s) from ServerConfig(s).
func NewEndpoints(id uint32, msp string, configs ...*connection.ServerConfig) []*commontypes.OrdererEndpoint {
	ordererEndpoints := make([]*commontypes.OrdererEndpoint, len(configs))
	for i, c := range configs {
		ordererEndpoints[i] = &commontypes.OrdererEndpoint{
			Host:  c.Endpoint.Host,
			Port:  c.Endpoint.Port,
			ID:    id,
			MspID: msp,
			API:   []string{Broadcast, Deliver},
		}
	}
	return ordererEndpoints
}
