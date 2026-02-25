/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testcrypto

import (
	"os"
	"path"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
)

// GetPeersIdentities returns the peers' identities from a crypto path.
func GetPeersIdentities(cryptoPath string) ([]msp.SigningIdentity, error) {
	return GetSigningIdentities(GetPeersMspDirs(cryptoPath)...)
}

// GetConsenterIdentities returns the orderer consenters identities from a crypto path.
func GetConsenterIdentities(cryptoPath string) ([]msp.SigningIdentity, error) {
	return GetSigningIdentities(GetOrdererMspDirs(cryptoPath)...)
}

// GetSigningIdentities loads signing identities from the given MSP directories.
func GetSigningIdentities(mspDirs ...*msp.DirLoadParameters) ([]msp.SigningIdentity, error) {
	identities := make([]msp.SigningIdentity, len(mspDirs))
	for i, mspDir := range mspDirs {
		localMsp, err := msp.LoadLocalMspDir(*mspDir)
		if err != nil {
			return nil, err
		}
		identities[i], err = localMsp.GetDefaultSigningIdentity()
		if err != nil {
			return nil, errors.Wrap(err, "loading signing identity")
		}
	}
	return identities, nil
}

// GetPeersMspDirs returns the peers' MSP directory path.
func GetPeersMspDirs(cryptoPath string) []*msp.DirLoadParameters {
	peerOrgPath := path.Join(cryptoPath, cryptogen.PeerOrganizationsDir)
	peerMspDirs := GetMspDirs(peerOrgPath)
	for _, mspItem := range peerMspDirs {
		clientName := "client@" + mspItem.MspName + ".com"
		mspItem.MspDir = path.Join(mspItem.MspDir, "users", clientName, "msp")
	}
	return peerMspDirs
}

// GetOrdererMspDirs returns the orderers' MSP directory path.
func GetOrdererMspDirs(cryptoPath string) []*msp.DirLoadParameters {
	ordererOrgPath := path.Join(cryptoPath, cryptogen.OrdererOrganizationsDir)
	ordererMspDirs := GetMspDirs(ordererOrgPath)
	for _, mspItem := range ordererMspDirs {
		nodeName := "consenter-" + mspItem.MspName[len("orderer-"):]
		mspItem.MspDir = path.Join(mspItem.MspDir, "orderers", nodeName, "msp")
	}
	return ordererMspDirs
}

// GetMspDirs returns the MSP dir parameter per organization in the path.
func GetMspDirs(targetPath string) []*msp.DirLoadParameters {
	dir, err := os.ReadDir(targetPath)
	if err != nil {
		return nil
	}
	mspDirs := make([]*msp.DirLoadParameters, 0, len(dir))
	for _, dirEntry := range dir {
		if !dirEntry.IsDir() {
			continue
		}
		orgName := dirEntry.Name()
		mspDirs = append(mspDirs, &msp.DirLoadParameters{
			MspName: orgName,
			MspDir:  path.Join(targetPath, orgName),
		})
	}
	return mspDirs
}
