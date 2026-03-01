/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// ClientTLSConfigsPerOrg holds each organization's representative TLS config for future client creation.
// We use this struct for the TestWithDynamicRootCAs.
type ClientTLSConfigsPerOrg struct {
	Peer map[string]connection.TLSConfig
	YAML map[string]connection.TLSConfig
}

// BuildClientTLSConfigsPerOrg builds TLS configs using only the "client" user.
func BuildClientTLSConfigsPerOrg(t *testing.T, root string, yamlConfig connection.TLSConfig) *ClientTLSConfigsPerOrg {
	t.Helper()

	peerRoot := filepath.Join(root, cryptogen.PeerOrganizationsDir)
	peerConfigs := make(map[string]connection.TLSConfig)

	orgEntries, err := os.ReadDir(peerRoot)
	// If the path doesn't exist, return empty maps to avoid nil pointer issues
	if err != nil {
		return &ClientTLSConfigsPerOrg{
			Peer: make(map[string]connection.TLSConfig),
			YAML: make(map[string]connection.TLSConfig),
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
		YAML: map[string]connection.TLSConfig{
			"org0": yamlConfig,
		},
	}
}
