/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric/msp"
	mspmgmt "github.com/hyperledger/fabric/msp/mgmt"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

var logger = logging.New("orderer-connection")

// NewIdentitySigner instantiate a signer for the given identity.
func NewIdentitySigner(config *IdentityConfig) (msp.SigningIdentity, error) { //nolint:ireturn,nolintlint // bug.
	logger.Infof("Initialize signer: %s", &utils.LazyJSON{O: config, Indent: "  "})
	if config == nil || !config.SignedEnvelopes || config.MspID == "" || config.MSPDir == "" {
		logger.Infof("Skipping signer initialization")
		return nil, nil
	}

	mspConfig, err := msp.GetLocalMspConfig(config.MSPDir, config.BCCSP, config.MspID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load MSP config")
	}
	err = mspmgmt.GetLocalMSP(factory.GetDefault()).Setup(mspConfig)
	if err != nil { // Handle errors reading the config file
		return nil, errors.Wrap(err, "failed to initialize local MSP")
	}

	signer, err := mspmgmt.GetLocalMSP(factory.GetDefault()).GetDefaultSigningIdentity()
	if err != nil {
		return nil, errors.Wrap(err, "failed to load local signing identity")
	}
	return signer, nil
}
