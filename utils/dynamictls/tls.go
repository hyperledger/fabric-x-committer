/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dynamictls

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/protoutil"
)

var logger = flogging.MustGetLogger("dynamic-root-ca")

// NewApplicationRootCAsFromConfigBlock retrieves the root CA certificates of all application orgs from a config block.
func NewApplicationRootCAsFromConfigBlock(configBlock *common.Block) ([][]byte, error) {
	logger.Debug("reading application root CAs from config block")
	envelope, err := protoutil.ExtractEnvelope(configBlock, 0)
	if err != nil {
		return nil, errors.Wrap(err, "failed to extract envelope")
	}
	return NewOrganizationsFromEnvelope(envelope)
}

// NewOrganizationsFromEnvelope retrieves the root CA certificates of all application orgs from a config transaction.
func NewOrganizationsFromEnvelope(envelope *common.Envelope) ([][]byte, error) {
	logger.Debug("reading application root CAs from envelope")
	bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
	if err != nil {
		return nil, errors.Wrap(err, "failed to create config bundle")
	}
	app, ok := bundle.ApplicationConfig()
	if !ok {
		return nil, errors.New("could not find application config")
	}

	var allOrganizationsRootCAs [][]byte
	for orgID, orgData := range app.Organizations() {
		logger.Debugf("reading org %s", orgID)
		allOrganizationsRootCAs = append(allOrganizationsRootCAs, orgData.MSP().GetTLSRootCerts()...)
	}

	return allOrganizationsRootCAs, nil
}
