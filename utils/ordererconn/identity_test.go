/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn_test

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

func TestNewIdentitySigner(t *testing.T) {
	t.Parallel()

	peerMspDir := initIdentityTestEnv(t)

	for _, tc := range []struct {
		name   string
		config *ordererconn.IdentityConfig
	}{
		{
			name:   "nil config returns nil without error",
			config: nil,
		},
		{
			name: "empty MspID returns nil without error",
			config: &ordererconn.IdentityConfig{
				MspID:  "",
				MSPDir: peerMspDir.MspDir,
			},
		},
		{
			name: "empty MSPDir returns nil without error",
			config: &ordererconn.IdentityConfig{
				MspID:  "test-msp",
				MSPDir: "",
			},
		},
		{
			name: "both MspID and MSPDir empty returns nil without error",
			config: &ordererconn.IdentityConfig{
				MspID:  "",
				MSPDir: "",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			signer, err := ordererconn.NewIdentitySigner(tc.config)
			require.NoError(t, err)
			require.Nil(t, signer)
		})
	}

	for _, tc := range []struct {
		name   string
		config *ordererconn.IdentityConfig
	}{
		{
			name:   "valid peer MSP config",
			config: mspDirToIdentityConfig(peerMspDir),
		},
		{
			name: "valid peer MSP config with BCCSP",
			config: &ordererconn.IdentityConfig{
				MspID:  peerMspDir.MspName,
				MSPDir: peerMspDir.MspDir,
				BCCSP:  factory.GetDefaultOpts(),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			signer, err := ordererconn.NewIdentitySigner(tc.config)
			require.NoError(t, err)
			require.NotNil(t, signer)
			// Verify the signer is functional
			require.NotEmpty(t, signer.GetMSPIdentifier())
			serialized, serErr := signer.Serialize()
			require.NoError(t, serErr)
			require.NotEmpty(t, serialized)
		})
	}

	corruptedPeerMspDir := initIdentityTestEnv(t)
	require.NoError(t, os.RemoveAll(filepath.Join(corruptedPeerMspDir.MspDir, "keystore")))
	for _, tc := range []struct {
		name   string
		config *ordererconn.IdentityConfig
	}{
		{
			name: "non-existent MSP directory",
			config: &ordererconn.IdentityConfig{
				MspID:  "test-msp",
				MSPDir: path.Join(t.TempDir(), "none"),
			},
		},
		{
			name: "invalid MSP directory structure",
			config: &ordererconn.IdentityConfig{
				MspID:  "test-msp",
				MSPDir: t.TempDir(), // Valid directory but not an MSP directory
			},
		},
		{
			name:   "corrupted MSP directory structure",
			config: mspDirToIdentityConfig(corruptedPeerMspDir),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			signer, err := ordererconn.NewIdentitySigner(tc.config)
			require.Error(t, err)
			require.Nil(t, signer)
		})
	}
}

func initIdentityTestEnv(t *testing.T) *msp.DirLoadParameters {
	t.Helper()
	// Create test crypto material.
	cryptoDir := t.TempDir()
	_, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir, &testcrypto.ConfigBlock{
		ChannelID:             "test-channel",
		PeerOrganizationCount: 1,
	})
	require.NoError(t, err)

	// Get MSP directories
	peerMspDirs := testcrypto.GetPeersMspDirs(cryptoDir)
	require.Len(t, peerMspDirs, 1)
	return peerMspDirs[0]
}

func mspDirToIdentityConfig(d *msp.DirLoadParameters) *ordererconn.IdentityConfig {
	return &ordererconn.IdentityConfig{
		MspID:  d.MspName,
		MSPDir: d.MspDir,
	}
}
