/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testcrypto

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/require"
)

// TestCreateOrExtendConfigBlockWithCrypto tests the creation of config blocks with crypto material.
func TestCreateOrExtendConfigBlockWithCrypto(t *testing.T) { //nolint:gocognit
	t.Parallel()

	for _, tc := range []struct {
		name             string
		peerOrgCount     uint32
		ordererEndpoints []*types.OrdererEndpoint
	}{
		{
			name:             "single peer and orderer org",
			peerOrgCount:     1,
			ordererEndpoints: []*types.OrdererEndpoint{{ID: 0, Host: "localhost", Port: 7050}},
		},
		{
			name:             "multiple peer orgs",
			peerOrgCount:     3,
			ordererEndpoints: []*types.OrdererEndpoint{{ID: 0, Host: "localhost", Port: 7050}},
		},
		{
			name:         "multiple orderer orgs",
			peerOrgCount: 1,
			ordererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "orderer1.org0.com", Port: 7050},
				{ID: 1, Host: "orderer1.org1.com", Port: 7051},
				{ID: 2, Host: "orderer1.org2.com", Port: 7052},
			},
		},
		{
			name:         "multiple orderer orgs with two IDs",
			peerOrgCount: 1,
			ordererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "orderer1.org0.com", Port: 7050},
				{ID: 0, Host: "orderer1.org0.com", Port: 7051},
				{ID: 2, Host: "orderer1.org2.com", Port: 7052},
			},
		},
		{
			name:             "zero peer orgs",
			peerOrgCount:     0,
			ordererEndpoints: []*types.OrdererEndpoint{{ID: 0, Host: "localhost", Port: 7050}},
		},
		{
			name:             "no orderer endpoints uses default",
			peerOrgCount:     1,
			ordererEndpoints: []*types.OrdererEndpoint{},
		},
		{
			name:         "orderer with broadcast API only",
			peerOrgCount: 1,
			ordererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "router.org0.com", Port: 7050, API: []string{types.Broadcast}},
			},
		},
		{
			name:         "orderer with deliver API only",
			peerOrgCount: 1,
			ordererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "assembler.org0.com", Port: 7050, API: []string{types.Deliver}},
			},
		},
		{
			name:         "mixed orderer APIs",
			peerOrgCount: 1,
			ordererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "router.org0.com", Port: 7050, API: []string{types.Broadcast}},
				{ID: 0, Host: "assembler.org0.com", Port: 7051, API: []string{types.Deliver}},
				{ID: 0, Host: "orderer.org0.com", Port: 7052, API: []string{types.Broadcast, types.Deliver}},
			},
		},
		{
			name:         "multiple orgs with separated APIs",
			peerOrgCount: 2,
			ordererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "router.org0.com", Port: 7050, API: []string{types.Broadcast}},
				{ID: 1, Host: "assembler.org1.com", Port: 7051, API: []string{types.Deliver}},
				{ID: 2, Host: "orderer.org2.com", Port: 7052},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cryptoPath := t.TempDir()

			block, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
				ChannelID:             "test-channel",
				PeerOrganizationCount: tc.peerOrgCount,
				OrdererEndpoints:      tc.ordererEndpoints,
			})
			require.NoError(t, err)
			require.NotNil(t, block)
			require.NotNil(t, block.Header)

			// Calculate expected orderer orgs from endpoints
			ordererOrgsMap := make(map[uint32]bool)
			for _, e := range tc.ordererEndpoints {
				ordererOrgsMap[e.ID] = true
			}
			expectedOrdererOrgs := len(ordererOrgsMap)
			if expectedOrdererOrgs == 0 {
				expectedOrdererOrgs = 1 // default orderer org
			}

			// Verify peer organizations
			peerOrgPath := filepath.Join(cryptoPath, cryptogen.PeerOrganizationsDir)
			if tc.peerOrgCount > 0 {
				require.DirExists(t, peerOrgPath)
				peerEntries, peerErr := os.ReadDir(peerOrgPath)
				require.NoError(t, peerErr)
				require.Len(t, peerEntries, int(tc.peerOrgCount))
			} else {
				if _, statErr := os.Stat(peerOrgPath); statErr == nil {
					peerEntries, readErr := os.ReadDir(peerOrgPath)
					require.NoError(t, readErr)
					require.Empty(t, peerEntries)
				}
			}

			// Verify orderer organizations
			ordererOrgPath := filepath.Join(cryptoPath, cryptogen.OrdererOrganizationsDir)
			require.DirExists(t, ordererOrgPath)
			ordererEntries, ordererErr := os.ReadDir(ordererOrgPath)
			require.NoError(t, ordererErr)
			require.Len(t, ordererEntries, expectedOrdererOrgs)
		})
	}

	t.Run("verify node names for separated APIs", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()

		_, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 1,
			OrdererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "router.org0.com", Port: 7050, API: []string{types.Broadcast}},
				{ID: 0, Host: "assembler.org0.com", Port: 7051, API: []string{types.Deliver}},
				{ID: 0, Host: "orderer.org0.com", Port: 7052, API: []string{types.Broadcast, types.Deliver}},
			},
		})
		require.NoError(t, err)

		// Verify orderer node directories exist with correct names
		ordererOrgPath := filepath.Join(cryptoPath, cryptogen.OrdererOrganizationsDir, "orderer-org-0", "orderers")
		require.DirExists(t, ordererOrgPath)

		entries, err := os.ReadDir(ordererOrgPath)
		require.NoError(t, err)
		require.Len(t, entries, 4, "Expected 3 orderer nodes + 1 consenter node")

		// Check that the node names match the expected pattern
		nodeNames := make(map[string]any)
		for _, entry := range entries {
			if entry.IsDir() {
				nodeNames[entry.Name()] = nil
			}
		}

		require.Contains(t, nodeNames, "router-0-org-0", "Expected router node for broadcast-only API")
		require.Contains(t, nodeNames, "assembler-1-org-0", "Expected assembler node for deliver-only API")
		require.Contains(t, nodeNames, "orderer-2-org-0", "Expected orderer node for both APIs")
		require.Contains(t, nodeNames, "consenter-org-0", "Expected consenter node")
	})

	t.Run("overwrite existing crypto material with different config", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()

		// Create initial config with 1 peer org
		block1, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 1,
			OrdererEndpoints:      []*types.OrdererEndpoint{{ID: 0, Host: "localhost", Port: 7050}},
		})
		require.NoError(t, err)
		require.NotNil(t, block1)

		// Verify initial state
		peerOrgPath := filepath.Join(cryptoPath, cryptogen.PeerOrganizationsDir)
		entries1, err := os.ReadDir(peerOrgPath)
		require.NoError(t, err)
		require.Len(t, entries1, 1)

		// Create second config with 3 peer orgs (should overwrite)
		block2, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 3,
			OrdererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "orderer1.example.com", Port: 7050},
				{ID: 1, Host: "orderer2.example.com", Port: 7051},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, block2)

		// Verify overwritten state has 3 peer orgs
		entries2, err := os.ReadDir(peerOrgPath)
		require.NoError(t, err)
		require.Len(t, entries2, 3)

		// Verify orderer orgs were also updated (2 unique IDs)
		ordererOrgPath := filepath.Join(cryptoPath, cryptogen.OrdererOrganizationsDir)
		ordererEntries, err := os.ReadDir(ordererOrgPath)
		require.NoError(t, err)
		require.Len(t, ordererEntries, 2)
	})
}

// TestIdentityLoading tests loading peer and consenter identities from crypto material.
func TestIdentityLoading(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		peerOrgCount     uint32
		ordererEndpoints []*types.OrdererEndpoint
	}{
		{
			name:             "single peer and consenter",
			peerOrgCount:     1,
			ordererEndpoints: []*types.OrdererEndpoint{{ID: 0, Host: "localhost", Port: 7050}},
		},
		{
			name:         "multiple peers and consenters",
			peerOrgCount: 3,
			ordererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "orderer1.example.com", Port: 7050},
				{ID: 1, Host: "orderer2.example.com", Port: 7051},
				{ID: 2, Host: "orderer3.example.com", Port: 7052},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cryptoPath := t.TempDir()

			_, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
				ChannelID:             "test-channel",
				PeerOrganizationCount: tc.peerOrgCount,
				OrdererEndpoints:      tc.ordererEndpoints,
			})
			require.NoError(t, err)

			// Calculate expected consenter count from unique orderer org IDs
			ordererOrgsMap := make(map[uint32]bool)
			for _, e := range tc.ordererEndpoints {
				ordererOrgsMap[e.ID] = true
			}
			expectedConsenters := len(ordererOrgsMap)

			// Test GetPeersIdentities
			peerIdentities, err := GetPeersIdentities(cryptoPath)
			require.NoError(t, err)
			require.Len(t, peerIdentities, int(tc.peerOrgCount))
			for _, id := range peerIdentities {
				require.NotNil(t, id)
				require.NotEmpty(t, id.GetMSPIdentifier())
			}

			// Test GetConsenterIdentities
			consenterIdentities, err := GetConsenterIdentities(cryptoPath)
			require.NoError(t, err)
			require.Len(t, consenterIdentities, expectedConsenters)
			for _, id := range consenterIdentities {
				require.NotNil(t, id)
				require.NotEmpty(t, id.GetMSPIdentifier())
			}
		})
	}

	t.Run("empty or non-existent paths", func(t *testing.T) {
		t.Parallel()

		// Non-existent path
		peerIDs, err := GetPeersIdentities("/non/existent/path")
		require.NoError(t, err)
		require.Empty(t, peerIDs)

		consenterIDs, err := GetConsenterIdentities("/non/existent/path")
		require.NoError(t, err)
		require.Empty(t, consenterIDs)

		// Empty directory
		emptyPath := t.TempDir()
		peerIDs, err = GetPeersIdentities(emptyPath)
		require.NoError(t, err)
		require.Empty(t, peerIDs)

		consenterIDs, err = GetConsenterIdentities(emptyPath)
		require.NoError(t, err)
		require.Empty(t, consenterIDs)
	})
}

// TestGetSigningIdentities tests loading signing identities from MSP directories.
func TestGetSigningIdentities(t *testing.T) {
	t.Parallel()

	t.Run("load from peer MSP directories", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()

		_, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 2,
		})
		require.NoError(t, err)

		mspDirs := GetPeersMspDirs(cryptoPath)
		require.Len(t, mspDirs, 2)

		identities, err := GetSigningIdentities(mspDirs...)
		require.NoError(t, err)
		require.Len(t, identities, 2)

		for _, id := range identities {
			require.NotNil(t, id)
			require.NotEmpty(t, id.GetMSPIdentifier())
		}
	})

	t.Run("load mixed peer and orderer identities", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()

		_, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 1,
			OrdererEndpoints:      []*types.OrdererEndpoint{{ID: 0, Host: "localhost", Port: 7050}},
		})
		require.NoError(t, err)

		peerMspDirs := GetPeersMspDirs(cryptoPath)
		ordererMspDirs := GetOrdererMspDirs(cryptoPath)
		allMspDirs := make([]*msp.DirLoadParameters, 0, len(peerMspDirs)+len(ordererMspDirs))
		allMspDirs = append(allMspDirs, peerMspDirs...)
		allMspDirs = append(allMspDirs, ordererMspDirs...)

		identities, err := GetSigningIdentities(allMspDirs...)
		require.NoError(t, err)
		require.Len(t, identities, 2)
	})

	t.Run("empty MSP directories", func(t *testing.T) {
		t.Parallel()
		identities, err := GetSigningIdentities()
		require.NoError(t, err)
		require.Empty(t, identities)
	})

	t.Run("invalid MSP directory", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()

		_, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 1,
		})
		require.NoError(t, err)

		mspDirs := GetPeersMspDirs(cryptoPath)
		require.Len(t, mspDirs, 1)

		// Corrupt MSP directory
		signcertsDir := filepath.Join(mspDirs[0].MspDir, cryptogen.SignCertsDir)
		err = os.RemoveAll(signcertsDir)
		require.NoError(t, err)

		identities, err := GetSigningIdentities(mspDirs...)
		require.Error(t, err)
		require.Nil(t, identities)
	})
}

// TestMspDirectoryRetrieval tests retrieving MSP directories for peers and orderers.
func TestMspDirectoryRetrieval(t *testing.T) {
	t.Parallel()

	t.Run("peer MSP directories", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()

		_, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 3,
		})
		require.NoError(t, err)

		mspDirs := GetPeersMspDirs(cryptoPath)
		require.Len(t, mspDirs, 3)

		for i, mspDir := range mspDirs {
			require.NotNil(t, mspDir)
			require.NotEmpty(t, mspDir.MspName)
			require.NotEmpty(t, mspDir.MspDir)
			require.Contains(t, mspDir.MspName, "peer-org-")
			require.Contains(t, mspDir.MspDir, "users")
			require.Contains(t, mspDir.MspDir, "client@")

			_, err := os.Stat(mspDir.MspDir)
			require.NoError(t, err, "MSP directory %d should exist", i)
		}
	})

	t.Run("orderer MSP directories", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()

		_, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 1,
			OrdererEndpoints: []*types.OrdererEndpoint{
				{ID: 0, Host: "orderer1.example.com", Port: 7050},
				{ID: 1, Host: "orderer2.example.com", Port: 7051},
				{ID: 2, Host: "orderer3.example.com", Port: 7052},
			},
		})
		require.NoError(t, err)

		mspDirs := GetOrdererMspDirs(cryptoPath)
		require.Len(t, mspDirs, 3)

		for i, mspDir := range mspDirs {
			require.NotNil(t, mspDir)
			require.NotEmpty(t, mspDir.MspName)
			require.NotEmpty(t, mspDir.MspDir)
			require.Contains(t, mspDir.MspName, "orderer-org-")
			require.Contains(t, mspDir.MspDir, "orderers")
			require.Contains(t, mspDir.MspDir, "consenter-")

			_, err := os.Stat(mspDir.MspDir)
			require.NoError(t, err, "MSP directory %d should exist", i)
		}
	})

	t.Run("edge cases", func(t *testing.T) {
		t.Parallel()

		// Non-existent paths
		require.Nil(t, GetPeersMspDirs("/non/existent/path"))
		require.Nil(t, GetOrdererMspDirs("/non/existent/path"))

		// Empty directories
		cryptoPath := t.TempDir()
		peerOrgPath := filepath.Join(cryptoPath, cryptogen.PeerOrganizationsDir)
		ordererOrgPath := filepath.Join(cryptoPath, cryptogen.OrdererOrganizationsDir)

		require.NoError(t, os.MkdirAll(peerOrgPath, 0o750))
		require.NoError(t, os.MkdirAll(ordererOrgPath, 0o750))

		require.Empty(t, GetPeersMspDirs(cryptoPath))
		require.Empty(t, GetOrdererMspDirs(cryptoPath))
	})
}

// TestGetMspDirs tests the generic MSP directory retrieval function.
func TestGetMspDirs(t *testing.T) {
	t.Parallel()

	t.Run("populated directory", func(t *testing.T) {
		t.Parallel()
		cryptoPath := t.TempDir()

		_, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
			ChannelID:             "test-channel",
			PeerOrganizationCount: 2,
		})
		require.NoError(t, err)

		peerOrgPath := filepath.Join(cryptoPath, cryptogen.PeerOrganizationsDir)
		mspDirs := GetMspDirs(peerOrgPath)

		require.Len(t, mspDirs, 2)
		for _, mspDir := range mspDirs {
			require.NotNil(t, mspDir)
			require.NotEmpty(t, mspDir.MspName)
			require.NotEmpty(t, mspDir.MspDir)
		}
	})

	t.Run("directory filtering", func(t *testing.T) {
		t.Parallel()
		dirPath := t.TempDir()

		// Create mixed content
		require.NoError(t, os.WriteFile(filepath.Join(dirPath, "file.txt"), []byte("content"), 0o600))
		require.NoError(t, os.Mkdir(filepath.Join(dirPath, "org1"), 0o750))
		require.NoError(t, os.Mkdir(filepath.Join(dirPath, "org2"), 0o750))
		require.NoError(t, os.Mkdir(filepath.Join(dirPath, "org3"), 0o750))

		mspDirs := GetMspDirs(dirPath)
		require.Len(t, mspDirs, 3)

		orgNames := make(map[string]any)
		for _, mspDir := range mspDirs {
			orgNames[mspDir.MspName] = nil
		}

		require.Contains(t, orgNames, "org1")
		require.Contains(t, orgNames, "org2")
		require.Contains(t, orgNames, "org3")
	})

	t.Run("edge cases", func(t *testing.T) {
		t.Parallel()

		// Non-existent path
		require.Nil(t, GetMspDirs("/non/existent/path"))

		// Empty directory
		emptyPath := t.TempDir()
		require.Empty(t, GetMspDirs(emptyPath))

		// Directory with only files
		filesOnlyPath := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(filesOnlyPath, "file1.txt"), []byte("content"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(filesOnlyPath, "file2.txt"), []byte("content"), 0o600))
		require.Empty(t, GetMspDirs(filesOnlyPath))
	})
}

// TestIntegrationCryptoWorkflow tests the complete workflow of creating and loading crypto material.
func TestIntegrationCryptoWorkflow(t *testing.T) {
	t.Parallel()

	cryptoPath := t.TempDir()

	// Create crypto material
	_, err := CreateOrExtendConfigBlockWithCrypto(cryptoPath, &ConfigBlock{
		ChannelID:             "integration-channel",
		PeerOrganizationCount: 2,
		OrdererEndpoints: []*types.OrdererEndpoint{
			{ID: 0, Host: "orderer1.example.com", Port: 7050},
			{ID: 1, Host: "orderer2.example.com", Port: 7051},
		},
	})
	require.NoError(t, err)

	// Load peer identities
	peerIdentities, err := GetPeersIdentities(cryptoPath)
	require.NoError(t, err)
	require.Len(t, peerIdentities, 2)

	// Load consenter identities
	consenterIdentities, err := GetConsenterIdentities(cryptoPath)
	require.NoError(t, err)
	require.Len(t, consenterIdentities, 2)

	// Verify all identities are valid
	for _, id := range peerIdentities {
		require.NotEmpty(t, id.GetMSPIdentifier())
		cert, err := id.GetCertificatePEM()
		require.NoError(t, err)
		require.NotEmpty(t, cert)
	}

	for _, id := range consenterIdentities {
		require.NotEmpty(t, id.GetMSPIdentifier())
		cert, err := id.GetCertificatePEM()
		require.NoError(t, err)
		require.NotEmpty(t, cert)
	}
}
