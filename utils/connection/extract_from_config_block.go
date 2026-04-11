/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/protoutil"
)

// GetApplicationRootCAsFromEnvelopeBytes extracts the TLS root certificates of all application orgs
// from a configuration envelope.
func GetApplicationRootCAsFromEnvelopeBytes(envelopeBytes []byte) ([][]byte, error) {
	logger.Debug("reading application root CAs from envelope")
	envelope, err := protoutil.UnmarshalEnvelope(envelopeBytes)
	if err != nil {
		return nil, err
	}
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
