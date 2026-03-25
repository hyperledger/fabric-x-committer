/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererdial

import (
	"math/rand/v2"

	"github.com/hyperledger/fabric-x-common/common/channelconfig"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

type (
	// ClientMaterial contains the connection material for an orderer.
	ClientMaterial struct {
		Joint             *connection.ClientMaterial
		PartyIDToMaterial map[uint32]*connection.ClientMaterial
	}

	// Parameters are the parameters to create connection material.
	// The API can be "deliver" or "broadcast".
	// In the committer, we only use "deliver" for production code (sidecar).
	// "broadcast" is used by the load generator to apply load on the orderer and for testing.
	Parameters struct {
		TLS   connection.TLSMaterials
		Retry *retry.Profile
		API   string
	}
)

// NewClientMaterial returns the client connection material given the config block material and the given parameters.
func NewClientMaterial(m *channelconfig.ConfigBlockMaterial, p Parameters) *ClientMaterial {
	res := &ClientMaterial{
		Joint: &connection.ClientMaterial{
			TLS:   p.TLS,
			Retry: p.Retry,
		},
		PartyIDToMaterial: make(map[uint32]*connection.ClientMaterial),
	}
	for _, org := range m.OrdererOrganizations {
		var caCerts [][]byte
		if len(p.TLS.Mode) > 0 && p.TLS.Mode != connection.NoneTLSMode && len(org.CACerts) > 0 {
			caCerts = org.CACerts
		}

		endpoints := make([]*connection.Endpoint, 0, len(org.Endpoints))
		for _, ep := range org.Endpoints {
			if !ep.SupportsAPI(p.API) {
				continue
			}
			perID, ok := res.PartyIDToMaterial[ep.ID]
			if !ok {
				perID = &connection.ClientMaterial{
					TLS:   p.TLS,
					Retry: p.Retry,
				}
				perID.TLS.CACerts = append(perID.TLS.CACerts, caCerts...)
				res.PartyIDToMaterial[ep.ID] = perID
			}
			connEp := &connection.Endpoint{Host: ep.Host, Port: ep.Port}

			endpoints = append(endpoints, connEp)
			perID.Endpoints = append(perID.Endpoints, connEp)
		}
		if len(endpoints) == 0 {
			continue
		}
		res.Joint.Endpoints = append(res.Joint.Endpoints, endpoints...)
		res.Joint.TLS.CACerts = append(res.Joint.TLS.CACerts, caCerts...)
	}

	// We shuffle the endpoints for load balancing.
	shuffle(res.Joint.Endpoints)
	for _, mat := range res.PartyIDToMaterial {
		shuffle(mat.Endpoints)
	}
	return res
}

func shuffle[T any](nodes []T) {
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
}
