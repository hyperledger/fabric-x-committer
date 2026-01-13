/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"os"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// OrganizationMaterial contains the MspID (Organization ID), orderer endpoints, and their root CAs in bytes.
type OrganizationMaterial struct {
	MspID     string
	Endpoints []*commontypes.OrdererEndpoint
	CACerts   [][]byte
}

// NewOrganizationsMaterials reads the organizations' materials.
func NewOrganizationsMaterials(orgs map[string]*OrganizationConfig, tlsMode string) ([]*OrganizationMaterial, error) {
	organizationsMaterial := make([]*OrganizationMaterial, 0, len(orgs))
	for mspID, orgConfig := range orgs {
		orgsMaterial := &OrganizationMaterial{
			MspID:     mspID,
			Endpoints: orgConfig.Endpoints,
		}
		organizationsMaterial = append(organizationsMaterial, orgsMaterial)
		if tlsMode == connection.NoneTLSMode || tlsMode == connection.UnmentionedTLSMode {
			continue
		}
		for _, caPath := range orgConfig.CACerts {
			caBytes, err := os.ReadFile(caPath)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to load CA certificate from %s", caBytes)
			}
			orgsMaterial.CACerts = append(orgsMaterial.CACerts, caBytes)
		}
	}
	return organizationsMaterial, nil
}

// NewOrganizationsMaterialsFromConfigBlock retrieve the organization materials from a config block.
func NewOrganizationsMaterialsFromConfigBlock(configBlock *common.Block) ([]*OrganizationMaterial, error) {
	envelope, err := protoutil.ExtractEnvelope(configBlock, 0)
	if err != nil {
		return nil, errors.Wrap(err, "failed to extract envelope")
	}
	return NewOrganizationsMaterialsFromEnvelope(envelope)
}

// NewOrganizationsMaterialsFromEnvelope retrieve the organization materials from a config transaction.
func NewOrganizationsMaterialsFromEnvelope(envelope *common.Envelope) ([]*OrganizationMaterial, error) {
	bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
	if err != nil {
		return nil, errors.Wrap(err, "failed to create config bundle")
	}
	ordererCfg, ok := bundle.OrdererConfig()
	if !ok {
		return nil, errors.New("could not find orderer config")
	}
	organizationMaterials := make([]*OrganizationMaterial, 0, len(ordererCfg.Organizations()))
	for orgID, org := range ordererCfg.Organizations() {
		var endpoints []*commontypes.OrdererEndpoint
		endpointsStr := org.Endpoints()
		for _, eStr := range endpointsStr {
			e, err := commontypes.ParseOrdererEndpoint(eStr)
			if err != nil {
				return nil, err
			}
			e.MspID = orgID
			endpoints = append(endpoints, e)
		}
		organizationMaterials = append(organizationMaterials, &OrganizationMaterial{
			MspID:     orgID,
			Endpoints: endpoints,
			CACerts:   org.MSP().GetTLSRootCerts(),
		})
	}
	return organizationMaterials, nil
}
