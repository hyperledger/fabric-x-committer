/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testcrypto

import (
	"fmt"
	"os"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
)

// ConfigBlock represents the configuration of the config block.
type ConfigBlock struct {
	ChannelID             string
	OrdererEndpoints      []*types.OrdererEndpoint
	PeerOrganizationCount uint32
}

// CreateOrExtendConfigBlockWithCrypto creates a config block with crypto material.
// This will generate a new config block, overwriting the existing config block if it already exists.
// For each of the given ID in the OrdererEndpoint, we create an orderer organization "orderer-org-<ID>"
// with one consenter node "consenter-org-<ID>", and for each endpoint, an orderer node "orderer-<endpoint-index>-<ID>".
// For the given PeerOrganizationCount, we create that many peer organizations "peer-org-<index>"
// with one peer node each "sidecar-peer-org-<index>".
func CreateOrExtendConfigBlockWithCrypto(targetPath string, conf *ConfigBlock) (*common.Block, error) {
	ordererOrgsMap := make(map[uint32][]*types.OrdererEndpoint)
	for _, e := range conf.OrdererEndpoints {
		ordererOrgsMap[e.ID] = append(ordererOrgsMap[e.ID], e)
	}

	if len(ordererOrgsMap) == 0 {
		// We need at least one orderer org to create a config block.
		ordererOrgsMap[0] = []*types.OrdererEndpoint{{Host: "localhost", Port: 7050}}
	}

	orgs := make([]cryptogen.OrganizationParameters, 0, int(conf.PeerOrganizationCount)+len(conf.OrdererEndpoints))
	for orgID, endpoints := range ordererOrgsMap {
		ordererEndpoints := make([]*types.OrdererEndpoint, len(endpoints))
		ordererNodes := make([]cryptogen.Node, len(endpoints))
		for epIdx, e := range endpoints {
			var name string
			switch {
			case len(e.API) == 1 && e.API[0] == types.Broadcast:
				name = "router"
			case len(e.API) == 1 && e.API[0] == types.Deliver:
				name = "assembler"
			default:
				name = "orderer"
			}
			commonName := fmt.Sprintf("%s-%d-org-%d", name, epIdx, orgID)
			ordererNodes[epIdx] = cryptogen.Node{
				CommonName: commonName,
				Hostname:   fmt.Sprintf("%s.com", commonName),
				SANS:       []string{e.Host},
			}
			ordererEndpoints[epIdx] = e
		}
		orgs = append(orgs, cryptogen.OrganizationParameters{
			Name:             fmt.Sprintf("orderer-org-%d", orgID),
			Domain:           fmt.Sprintf("orderer-org-%d.com", orgID),
			OrdererEndpoints: ordererEndpoints,
			ConsenterNodes: []cryptogen.Node{{
				CommonName: fmt.Sprintf("consenter-org-%d", orgID),
				Hostname:   fmt.Sprintf("consenter-org-%d.com", orgID),
			}},
			OrdererNodes: ordererNodes,
		})
	}

	for i := range conf.PeerOrganizationCount {
		orgs = append(orgs, cryptogen.OrganizationParameters{
			Name:   fmt.Sprintf("peer-org-%d", i),
			Domain: fmt.Sprintf("peer-org-%d.com", i),
			PeerNodes: []cryptogen.Node{{
				CommonName: fmt.Sprintf("sidecar-peer-org-%d", i),
				Hostname:   fmt.Sprintf("sidecar-peer-org-%d.com", i),
			}},
		})
	}

	err := os.MkdirAll(targetPath, 0o750)
	if err != nil {
		return nil, errors.Wrap(err, "error creating crypto material folder")
	}

	// We create the config block on the basis of the default Fabric X config profile.
	// It will use the default parameters defined in this profile (e.g., Policies, OrdererType, etc.).
	// The generated organizations will use the default parameters taken from the first orderer org defined
	// in the profile.
	return cryptogen.CreateOrExtendConfigBlockWithCrypto(cryptogen.ConfigBlockParameters{
		TargetPath:    targetPath,
		BaseProfile:   configtxgen.SampleFabricX,
		ChannelID:     conf.ChannelID,
		Organizations: orgs,
	})
}
