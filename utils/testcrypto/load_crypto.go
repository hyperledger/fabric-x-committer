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
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
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

// ClientTLSConfigsPerOrg holds each organization's representative TLS config for future client creation.
// We use this struct for the TestWithDynamicRootCAs.
type ClientTLSConfigsPerOrg struct {
	Peer map[string]connection.TLSConfig
}

// BuildClientTLSConfigsPerOrg builds TLS configs using only the "client" user.
func BuildClientTLSConfigsPerOrg(t *testing.T, root string) *ClientTLSConfigsPerOrg {
	t.Helper()

	peerRoot := filepath.Join(root, cryptogen.PeerOrganizationsDir)
	peerConfigs := make(map[string]connection.TLSConfig)

	orgEntries, err := os.ReadDir(peerRoot)
	// If the path doesn't exist, return empty maps to avoid nil pointer issues
	if err != nil {
		return &ClientTLSConfigsPerOrg{
			Peer: make(map[string]connection.TLSConfig),
		}
	}

	// go over all peer organizations
	for _, orgEntry := range orgEntries {
		if !orgEntry.IsDir() {
			continue
		}

		// each peer has a dedicated directory.
		orgName := orgEntry.Name()
		orgDir := filepath.Join(peerRoot, orgName)

		// get the users directory of the current peer organization.
		usersDir := filepath.Join(orgDir, cryptogen.UsersDir)
		require.DirExists(t, usersDir, "missing users dir for org %s", orgName)

		userEntries, err := os.ReadDir(usersDir)
		require.NoError(t, err)

		var clientUser string
		for _, u := range userEntries {
			// Look for a directory starting with "client" and skip "Admin"
			// It's enough to get only one of them if there's more than 1, since they share the same root CA.
			if u.IsDir() && strings.HasPrefix(strings.ToLower(u.Name()), "client") {
				clientUser = u.Name()
				break
			}
		}
		// we must find a client user
		require.NotEmpty(t, clientUser, "no 'client' user directory found under %s", usersDir)

		// Define paths relative to the selected client user
		clientTLSDir := filepath.Join(usersDir, clientUser, cryptogen.TLSDir)
		peerConfigs[orgName] = connection.TLSConfig{
			Mode:        connection.MutualTLSMode,
			CertPath:    filepath.Join(clientTLSDir, "client.crt"),
			KeyPath:     filepath.Join(clientTLSDir, "client.key"),
			CACertPaths: []string{filepath.Join(clientTLSDir, "ca.crt")},
		}
	}

	return &ClientTLSConfigsPerOrg{
		Peer: peerConfigs,
	}
}
