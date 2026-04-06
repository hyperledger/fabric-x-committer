/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testcrypto

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
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

// GetOrdererConnConfig returns the configuration for an orderer connection using the config block and peer
// organizations in tha artifacts path.
func GetOrdererConnConfig(artifactsPath string, clientTLSConfig connection.TLSConfig) ordererdial.Config {
	peerMsp := GetPeersMspDirs(artifactsPath)
	var id *ordererdial.IdentityConfig
	if len(peerMsp) > 0 {
		id = &ordererdial.IdentityConfig{
			MspID:  peerMsp[0].MspName,
			MSPDir: peerMsp[0].MspDir,
			BCCSP:  peerMsp[0].CspConf,
		}
	}
	return ordererdial.Config{
		FaultToleranceLevel:        ordererdial.BFT,
		TLS:                        ordererdial.TLSConfigToOrdererTLSConfig(clientTLSConfig),
		LatestKnownConfigBlockPath: path.Join(artifactsPath, cryptogen.ConfigBlockFileName),
		Retry: &retry.Profile{
			InitialInterval: 10 * time.Millisecond,
			MaxInterval:     100 * time.Millisecond,
			Multiplier:      2,
			MaxElapsedTime:  time.Second,
		},
		Identity: id,
	}
}

// PerOrgTLSConfig holds each organization's representative TLS config for future client creation.
// We use this struct for the TestWithDynamicRootCAs.
type PerOrgTLSConfig struct {
	Peer map[string]connection.TLSConfig
}

// BuildClientTLSConfigsPerOrg builds TLS configs using only the "client" user.
func BuildClientTLSConfigsPerOrg(root string) (*PerOrgTLSConfig, error) {
	peerConfigs := make(map[string]connection.TLSConfig)
	peerRoot := filepath.Join(root, cryptogen.PeerOrganizationsDir)

	orgEntries, err := os.ReadDir(peerRoot)
	// If the path doesn't exist, return empty maps to avoid nil pointer issues
	if err != nil {
		return nil, errors.Newf("failed to read peer organizations dir: %v", peerRoot)
	}

	// go over all peer organizations
	for _, orgEntry := range orgEntries {
		if !orgEntry.IsDir() {
			continue
		}

		orgName := orgEntry.Name()
		usersDir := filepath.Join(peerRoot, orgName, cryptogen.UsersDir)

		// Extracted the inner loop logic to a helper function
		clientUser, err := getClientUser(usersDir)
		if err != nil {
			return nil, errors.Newf("org %s: %w", orgName, err)
		}

		// Define paths relative to the selected client user
		clientTLSDir := filepath.Join(usersDir, clientUser, cryptogen.TLSDir)
		peerConfigs[orgName] = connection.TLSConfig{
			Mode:        connection.MutualTLSMode,
			CertPath:    filepath.Join(clientTLSDir, "client.crt"),
			KeyPath:     filepath.Join(clientTLSDir, "client.key"),
			CACertPaths: []string{filepath.Join(clientTLSDir, "ca.crt")},
		}
	}

	return &PerOrgTLSConfig{
		Peer: peerConfigs,
	}, nil
}

// getClientUser scans the directory and returns the first client user directory name.
func getClientUser(usersDir string) (string, error) {
	userEntries, err := os.ReadDir(usersDir)
	if err != nil {
		return "", errors.Newf("missing or failed to read users dir: %e", err)
	}

	for _, u := range userEntries {
		if !u.IsDir() {
			continue
		}

		name := u.Name()
		if len(name) >= 6 && strings.EqualFold(name[:6], "client") {
			return name, nil
		}
	}

	return "", errors.Newf("no 'client' user directory found under %s", usersDir)
}
