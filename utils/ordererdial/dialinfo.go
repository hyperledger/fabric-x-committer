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
	// DialInfo contains the connection dial info for an orderer.
	DialInfo struct {
		Joint             *connection.DialInfo
		PartyIDToDialInfo map[uint32]*connection.DialInfo
	}

	// Parameters are the parameters to create connection dial info.
	// The API can be "deliver" or "broadcast".
	// In the committer, we only use "deliver" for production code (sidecar).
	// "broadcast" is used by the load generator to apply load on the orderer and for testing.
	Parameters struct {
		TLS   connection.TLSCredentials
		Retry *retry.Profile
		API   string
	}
)

// NewDialInfo returns the client connection dial info given the config block material and the given parameters.
func NewDialInfo(m *channelconfig.ConfigBlockMaterial, p Parameters) *DialInfo {
	res := &DialInfo{
		Joint: &connection.DialInfo{
			TLS:   p.TLS,
			Retry: p.Retry,
		},
		PartyIDToDialInfo: make(map[uint32]*connection.DialInfo),
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
			perID, ok := res.PartyIDToDialInfo[ep.ID]
			if !ok {
				perID = &connection.DialInfo{
					TLS:   p.TLS,
					Retry: p.Retry,
				}
				perID.TLS.CACerts = append(perID.TLS.CACerts, caCerts...)
				res.PartyIDToDialInfo[ep.ID] = perID
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
	for _, di := range res.PartyIDToDialInfo {
		shuffle(di.Endpoints)
	}
	return res
}

func shuffle[T any](nodes []T) {
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
}
