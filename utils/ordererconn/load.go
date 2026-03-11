/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"math/rand/v2"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/common/configtx"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// ConfigBlockMaterial contains the channel-ID, the config block, its bundle, and the organization's material.
	ConfigBlockMaterial struct {
		ChannelID                string
		ConfigBlock              *common.Block
		Bundle                   *channelconfig.Bundle
		OrdererOrganizations     []*OrdererOrganizationMaterial
		ApplicationOrganizations []*OrganizationMaterial
	}

	// ConnectionMaterial contains the connection material for an orderer.
	ConnectionMaterial struct {
		Joint *connection.ClientMaterial
		PerID map[uint32]*connection.ClientMaterial
	}

	// MaterialParameters are the parameters to create connection material.
	MaterialParameters struct {
		TLS   connection.TLSMaterials
		Retry *connection.RetryProfile
		API   string
	}

	// OrganizationMaterial contains the MspID (Organization ID), and its root CAs in bytes.
	OrganizationMaterial struct {
		MspID   string
		CACerts [][]byte
	}

	// OrdererOrganizationMaterial contains the MspID (Organization ID), orderer endpoints, and their root CAs in bytes.
	OrdererOrganizationMaterial struct {
		OrganizationMaterial
		Endpoints []*commontypes.OrdererEndpoint
	}
)

// ErrNotConfigBlock is returned when the block is not a config block.
var ErrNotConfigBlock = errors.New("the block is not a config block")

// LoadConfigBlockFromFile loads a config block from a file.
// If the block is not a config block, ErrNotConfigBlock will be returned.
func LoadConfigBlockFromFile(blockPath string) (*ConfigBlockMaterial, error) {
	if blockPath == "" {
		return nil, errors.New("config block path is empty")
	}
	configBlock, err := configtxgen.ReadBlock(blockPath)
	if err != nil {
		return nil, err
	}
	return LoadConfigBlock(configBlock)
}

// LoadConfigBlock attempts to read a config block from the given block.
// If the block is not a config block, ErrNotConfigBlock will be returned.
func LoadConfigBlock(block *common.Block) (*ConfigBlockMaterial, error) {
	// We expect config blocks to have exactly one transaction, with a valid payload.
	if block == nil || block.Data == nil || len(block.Data.Data) != 1 {
		return nil, ErrNotConfigBlock
	}
	configTx, err := protoutil.GetEnvelopeFromBlock(block.Data.Data[0])
	if err != nil {
		return nil, errors.Join(ErrNotConfigBlock, err)
	}

	payload, err := protoutil.UnmarshalPayload(configTx.Payload)
	if err != nil {
		return nil, errors.Join(ErrNotConfigBlock, err)
	}
	if payload.Header == nil {
		return nil, ErrNotConfigBlock
	}
	chHead, err := protoutil.UnmarshalChannelHeader(payload.Header.ChannelHeader)
	if err != nil || chHead.Type != int32(common.HeaderType_CONFIG) {
		return nil, errors.Join(ErrNotConfigBlock, err)
	}

	// This is a config block. Let's parse it.
	configEnvelope, err := configtx.UnmarshalConfigEnvelope(payload.Data)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshalling config envelope from payload data")
	}

	bundle, err := channelconfig.NewBundle(chHead.ChannelId, configEnvelope.Config, factory.GetDefault())
	if err != nil {
		return nil, errors.Wrap(err, "error creating channel config bundle")
	}
	ordererOrgs, err := newOrdererOrganizationsMaterialsFromBundle(bundle)
	if err != nil {
		return nil, err
	}
	applicationOrgs, err := newApplicationOrganizationsMaterialsFromBundle(bundle)
	if err != nil {
		return nil, err
	}
	return &ConfigBlockMaterial{
		ChannelID:                chHead.ChannelId,
		ConfigBlock:              block,
		Bundle:                   bundle,
		OrdererOrganizations:     ordererOrgs,
		ApplicationOrganizations: applicationOrgs,
	}, nil
}

// OrdererConnectionMaterial returns the connection material given the config block material and the given parameters.
func (m *ConfigBlockMaterial) OrdererConnectionMaterial(p MaterialParameters) *ConnectionMaterial {
	res := &ConnectionMaterial{
		Joint: &connection.ClientMaterial{
			TLS:   p.TLS,
			Retry: p.Retry,
		},
		PerID: make(map[uint32]*connection.ClientMaterial),
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
			perID, ok := res.PerID[ep.ID]
			if !ok {
				perID = &connection.ClientMaterial{
					TLS:   p.TLS,
					Retry: p.Retry,
				}
				perID.TLS.CACerts = append(perID.TLS.CACerts, caCerts...)
				res.PerID[ep.ID] = perID
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
	for _, mat := range res.PerID {
		shuffle(mat.Endpoints)
	}
	return res
}

// newOrdererOrganizationsMaterialsFromBundle reads the organizations' materials from a config block bundle.
func newOrdererOrganizationsMaterialsFromBundle(bundle *channelconfig.Bundle) ([]*OrdererOrganizationMaterial, error) {
	ordererCfg, ok := bundle.OrdererConfig()
	if !ok {
		return nil, errors.New("could not find orderer config")
	}
	orgs := ordererCfg.Organizations()
	orgsMaterial := newOrganizationsMaterials(orgs)

	ordererOrgMaterial := make([]*OrdererOrganizationMaterial, len(orgsMaterial))
	for i, org := range orgsMaterial {
		var endpoints []*commontypes.OrdererEndpoint
		endpointsStr := orgs[org.MspID].Endpoints()
		for _, eStr := range endpointsStr {
			e, err := commontypes.ParseOrdererEndpoint(eStr)
			if err != nil {
				return nil, err
			}
			e.MspID = org.MspID
			endpoints = append(endpoints, e)
		}
		ordererOrgMaterial[i] = &OrdererOrganizationMaterial{
			OrganizationMaterial: *org,
			Endpoints:            endpoints,
		}
	}
	return ordererOrgMaterial, nil
}

// newApplicationOrganizationsMaterialsFromBundle reads the organizations' materials from a config block bundle.
func newApplicationOrganizationsMaterialsFromBundle(bundle *channelconfig.Bundle) ([]*OrganizationMaterial, error) {
	applicationCfg, ok := bundle.ApplicationConfig()
	if !ok {
		return nil, errors.New("could not find application config")
	}
	return newOrganizationsMaterials(applicationCfg.Organizations()), nil
}

func newOrganizationsMaterials[T channelconfig.Org](orgs map[string]T) []*OrganizationMaterial {
	organizationMaterials := make([]*OrganizationMaterial, 0, len(orgs))
	for orgID, org := range orgs {
		organizationMaterials = append(organizationMaterials, &OrganizationMaterial{
			MspID:   orgID,
			CACerts: org.MSP().GetTLSRootCerts(),
		})
	}
	return organizationMaterials
}

func shuffle[T any](nodes []T) {
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
}
