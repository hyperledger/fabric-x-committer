/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"github.com/cockroachdb/errors"
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

	localMsp, err := msp.LoadLocalMspDir(msp.DirLoadParameters{
		MspName: config.MspID,
		MspDir:  config.MSPDir,
		CspConf: config.BCCSP,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create local MSP")
	}

	signer, err := localMsp.GetDefaultSigningIdentity()
	if err != nil {
		return nil, errors.Wrap(err, "failed to load local signing identity")
	}
	return signer, nil
}
