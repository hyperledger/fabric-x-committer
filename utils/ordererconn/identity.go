/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-x-common/msp"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

var logger = logging.New("orderer-connection")

// NewIdentitySigner instantiate a signer for the given identity.
func NewIdentitySigner(config *IdentityConfig) (msp.SigningIdentity, error) { //nolint:ireturn,nolintlint // bug.
	logger.Debugf("Initialize signer: %s", &utils.LazyJSON{O: config, Indent: "  "})
	if config == nil || config.MspID == "" || config.MSPDir == "" {
		logger.Debugf("Skipping signer initialization")
		return nil, nil
	}

	mspConfig, err := msp.GetLocalMspConfig(config.MSPDir, config.BCCSP, config.MspID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load MSP config")
	}

	bccspObj, err := getBCCSP(config.BCCSP)
	if err != nil {
		return nil, err
	}

	localMsp, err := msp.New(msp.Options[msp.ProviderTypeToString(msp.FABRIC)], bccspObj)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create local MSP")
	}
	err = localMsp.Setup(mspConfig)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize local MSP")
	}
	signer, err := localMsp.GetDefaultSigningIdentity()
	if err != nil {
		return nil, errors.Wrap(err, "failed to load local signing identity")
	}
	return signer, nil
}

func getBCCSP(opts *factory.FactoryOpts) (bccsp.BCCSP, error) { //nolint:ireturn // Inherited from Fabric.
	if opts == nil {
		return factory.GetDefault(), nil
	}
	bccspObj, err := factory.GetBCCSPFromOpts(opts)
	return bccspObj, errors.Wrap(err, "error creating BCCSP")
}
