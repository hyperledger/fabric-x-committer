/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn_test

import (
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

func TestLoadConfigBlockFromFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                    string
		path                    string
		expectedChannelID       string
		expectedOrdererOrgs     int
		expectedApplicationOrgs int
	}{
		{
			name:                    "valid config block file",
			path:                    createConfigBlockPath(t, "channel-0", 2, nil),
			expectedChannelID:       "channel-0",
			expectedOrdererOrgs:     1,
			expectedApplicationOrgs: 2,
		},
		{
			name:                    "valid config block with single peer org",
			path:                    createConfigBlockPath(t, "channel-1", 1, nil),
			expectedChannelID:       "channel-1",
			expectedOrdererOrgs:     1,
			expectedApplicationOrgs: 1,
		},
		{
			name: "valid config block with multiple peer orgs and orderers",
			path: createConfigBlockPath(t, "channel-2", 3, []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "orderer1.example.com", Port: 7050},
				{ID: 1, Host: "orderer2.example.com", Port: 7051},
			}),
			expectedChannelID:       "channel-2",
			expectedOrdererOrgs:     2,
			expectedApplicationOrgs: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			retMaterial, retErr := ordererconn.LoadConfigBlockFromFile(tc.path)

			require.NoError(t, retErr)
			require.NotNil(t, retMaterial)
			require.Equal(t, tc.expectedChannelID, retMaterial.ChannelID)
			require.NotNil(t, retMaterial.ConfigBlock)
			require.NotNil(t, retMaterial.Bundle)
			require.Len(t, retMaterial.OrdererOrganizations, tc.expectedOrdererOrgs)
			require.Len(t, retMaterial.ApplicationOrganizations, tc.expectedApplicationOrgs)
			for _, org := range retMaterial.OrdererOrganizations {
				require.NotEmpty(t, org.Endpoints)
			}
		})
	}
}

func TestLoadConfigBlockFromFileEdgeCases(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		blockPath     string
		expectedError string
	}{
		{
			name:          "empty path",
			blockPath:     "",
			expectedError: "config block path is empty",
		},
		{
			name:          "non-existent file",
			blockPath:     path.Join(t.TempDir(), "file.block"),
			expectedError: "could not read block",
		},
		{
			name:          "nil data",
			blockPath:     createBlockFile(t, &common.Block{}),
			expectedError: "the block is not a config block",
		},
		{
			name: "data block",
			blockPath: createBlockFile(t, &common.Block{
				Data: &common.BlockData{Data: [][]byte{[]byte("transaction data")}},
			}),
			expectedError: "the block is not a config block",
		},
		{
			name: "empty data",
			blockPath: createBlockFile(t, &common.Block{
				Data: &common.BlockData{Data: [][]byte{}},
			}),
			expectedError: "the block is not a config block",
		},
		{
			name: "multiple transactions",
			blockPath: createBlockFile(t, &common.Block{
				Data: &common.BlockData{
					Data: [][]byte{[]byte("transaction 1"), []byte("transaction 2")},
				},
			}),
			expectedError: "the block is not a config block",
		},
		{
			name: "invalid payload",
			blockPath: createBlockFile(t, &common.Block{
				Data: &common.BlockData{
					Data: [][]byte{
						protoutil.MarshalOrPanic(&common.Envelope{
							Payload: []byte("invalid payload"),
						}),
					},
				},
			}),
			expectedError: "the block is not a config block",
		},
		{
			name: "nil header",
			blockPath: createBlockFile(t, &common.Block{
				Data: &common.BlockData{
					Data: [][]byte{
						protoutil.MarshalOrPanic(&common.Envelope{
							Payload: protoutil.MarshalOrPanic(&common.Payload{Header: nil}),
						}),
					},
				},
			}),
			expectedError: "the block is not a config block",
		},
		{
			name: "wrong header type",
			blockPath: createBlockFile(t, &common.Block{
				Data: &common.BlockData{
					Data: [][]byte{
						protoutil.MarshalOrPanic(&common.Envelope{
							Payload: protoutil.MarshalOrPanic(&common.Payload{
								Header: &common.Header{
									ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{
										Type:      int32(common.HeaderType_ENDORSER_TRANSACTION),
										ChannelId: "test-channel",
									}),
								},
							}),
						}),
					},
				},
			}),
			expectedError: "the block is not a config block",
		},
		{
			name: "invalid config envelope",
			blockPath: createBlockFile(t, &common.Block{
				Data: &common.BlockData{
					Data: [][]byte{
						protoutil.MarshalOrPanic(&common.Envelope{
							Payload: protoutil.MarshalOrPanic(&common.Payload{
								Header: &common.Header{
									ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{
										Type:      int32(common.HeaderType_CONFIG),
										ChannelId: "test-channel",
									}),
								},
								Data: []byte("invalid config envelope"),
							}),
						}),
					},
				},
			}),
			expectedError: "error unmarshalling config envelope",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			retMaterial, retErr := ordererconn.LoadConfigBlockFromFile(tc.blockPath)
			require.ErrorContains(t, retErr, tc.expectedError)
			require.Nil(t, retMaterial)
		})
	}
}

func TestOrdererConnectionMaterial(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                       string
		endpoints                  []*commontypes.OrdererEndpoint
		params                     ordererconn.MaterialParameters
		expectedJointEndpointCount int
		expectedIDs                []uint32
		expectedEndpointCountPerID int
		expectCACerts              bool
	}{
		{
			name:                       "single orderer organization",
			endpoints:                  nil,
			params:                     ordererconn.MaterialParameters{API: commontypes.Deliver},
			expectedJointEndpointCount: 1,
			expectedEndpointCountPerID: 1,
			expectedIDs:                []uint32{0},
		},
		{
			name: "multiple orderer organizations",
			endpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "orderer1.org0.com", Port: 7050},
				{ID: 1, Host: "orderer1.org1.com", Port: 7051},
				{ID: 2, Host: "orderer1.org2.com", Port: 7052},
			},
			params:                     ordererconn.MaterialParameters{API: commontypes.Deliver},
			expectedJointEndpointCount: 3,
			expectedEndpointCountPerID: 1,
			expectedIDs:                []uint32{0, 1, 2},
		},
		{
			name: "organization with multiple endpoints",
			endpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "orderer1.org0.com", Port: 7050},
				{ID: 0, Host: "orderer2.org0.com", Port: 7051},
				{ID: 0, Host: "orderer3.org0.com", Port: 7052},
			},
			params:                     ordererconn.MaterialParameters{API: commontypes.Deliver},
			expectedJointEndpointCount: 3,
			expectedEndpointCountPerID: 3,
			expectedIDs:                []uint32{0},
		},
		{
			name: "filter by Broadcast API",
			endpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "router.org0.com", Port: 7050, API: []string{commontypes.Broadcast}},
				{ID: 0, Host: "assembler.org0.com", Port: 7051, API: []string{commontypes.Deliver}},
				{
					ID: 0, Host: "orderer.org0.com", Port: 7052,
					API: []string{commontypes.Broadcast, commontypes.Deliver},
				},
			},
			params:                     ordererconn.MaterialParameters{API: commontypes.Broadcast},
			expectedJointEndpointCount: 2,
			expectedEndpointCountPerID: 2,
			expectedIDs:                []uint32{0},
		},
		{
			name: "filter by Deliver API",
			endpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "router.org0.com", Port: 7050, API: []string{commontypes.Broadcast}},
				{ID: 0, Host: "assembler.org0.com", Port: 7051, API: []string{commontypes.Deliver}},
				{
					ID: 0, Host: "orderer.org0.com", Port: 7052,
					API: []string{commontypes.Broadcast, commontypes.Deliver},
				},
			},
			params:                     ordererconn.MaterialParameters{API: commontypes.Deliver},
			expectedJointEndpointCount: 2,
			expectedEndpointCountPerID: 2,
			expectedIDs:                []uint32{0},
		},
		{
			name:      "TLS mode server includes CA certs",
			endpoints: nil,
			params: ordererconn.MaterialParameters{
				TLS: connection.TLSMaterials{Mode: connection.OneSideTLSMode},
				API: commontypes.Deliver,
			},
			expectedJointEndpointCount: 1,
			expectedEndpointCountPerID: 1,
			expectedIDs:                []uint32{0},
			expectCACerts:              true,
		},
		{
			name:      "mTLS mode server includes CA certs",
			endpoints: nil,
			params: ordererconn.MaterialParameters{
				TLS: connection.TLSMaterials{Mode: connection.MutualTLSMode},
				API: commontypes.Deliver,
			},
			expectedJointEndpointCount: 1,
			expectedEndpointCountPerID: 1,
			expectedIDs:                []uint32{0},
			expectCACerts:              true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			material := createConfigBlockMaterial(t, 1, tc.endpoints)
			connMaterial := material.OrdererConnectionMaterial(tc.params)
			require.NotNil(t, connMaterial)
			require.Len(t, connMaterial.Joint.Endpoints, tc.expectedJointEndpointCount)
			require.ElementsMatch(t, tc.expectedIDs, slices.Collect(maps.Keys(connMaterial.PerID)))
			for _, m := range connMaterial.PerID {
				require.Len(t, m.Endpoints, tc.expectedEndpointCountPerID)
			}
			if tc.expectCACerts {
				require.NotEmpty(t, connMaterial.Joint.TLS.CACerts)
			} else {
				require.Empty(t, connMaterial.Joint.TLS.CACerts)
			}
		})
	}
}

func TestOrdererConnectionMaterialShuffling(t *testing.T) {
	t.Parallel()

	material := createConfigBlockMaterial(t, 1, []*commontypes.OrdererEndpoint{
		{ID: 0, Host: "orderer1.org0.com", Port: 7050},
		{ID: 0, Host: "orderer2.org0.com", Port: 7050},
		{ID: 0, Host: "orderer2.org0.com", Port: 7050},
		{ID: 1, Host: "orderer1.org1.com", Port: 7050},
		{ID: 1, Host: "orderer2.org1.com", Port: 7050},
		{ID: 1, Host: "orderer3.org1.com", Port: 7050},
		{ID: 2, Host: "orderer1.org2.com", Port: 7050},
		{ID: 2, Host: "orderer2.org2.com", Port: 7050},
		{ID: 2, Host: "orderer3.org2.com", Port: 7050},
	})

	params := ordererconn.MaterialParameters{
		TLS: connection.TLSMaterials{Mode: connection.NoneTLSMode},
		API: commontypes.Deliver,
	}

	jointOrders := make(map[string]any)
	perIDOrders := make(map[uint32]map[string]any)
	for i := range uint32(3) {
		perIDOrders[i] = make(map[string]any)
	}
	firstCall := material.OrdererConnectionMaterial(params)
	for range 10 {
		connMaterial := material.OrdererConnectionMaterial(params)
		require.ElementsMatch(t, firstCall.Joint.Endpoints, connMaterial.Joint.Endpoints)

		jointOrders[connection.AddressString(connMaterial.Joint.Endpoints...)] = nil
		for id, m := range connMaterial.PerID {
			require.ElementsMatch(t, firstCall.PerID[id].Endpoints, connMaterial.PerID[id].Endpoints)
			perIDOrders[id][connection.AddressString(m.Endpoints...)] = nil
		}
	}

	require.Greater(t, len(jointOrders), 1, "endpoints should be shuffled")
	for id, o := range perIDOrders {
		require.Greaterf(t, len(o), 1, "endpoints for ID %d should be shuffled", id)
	}
}

func TestOrganizationMaterialExtraction(t *testing.T) {
	t.Parallel()

	material := createConfigBlockMaterial(t, 3, []*commontypes.OrdererEndpoint{
		{ID: 0, Host: "orderer1.example.com", Port: 7050},
		{ID: 0, Host: "orderer2.example.com", Port: 7051},
	})

	require.Len(t, material.OrdererOrganizations, 1)
	ordererOrg := material.OrdererOrganizations[0]
	require.NotEmpty(t, ordererOrg.MspID)
	require.NotEmpty(t, ordererOrg.CACerts)
	require.Len(t, ordererOrg.Endpoints, 2)
	for _, ep := range ordererOrg.Endpoints {
		require.NotEmpty(t, ep.Host)
		require.NotZero(t, ep.Port)
		require.Equal(t, ordererOrg.MspID, ep.MspID)
	}

	require.Len(t, material.ApplicationOrganizations, 3)
	for _, peerOrg := range material.ApplicationOrganizations {
		require.NotEmpty(t, peerOrg.MspID)
		require.NotEmpty(t, peerOrg.CACerts)
	}
}

func TestParameterPropagation(t *testing.T) {
	t.Parallel()

	material := createConfigBlockMaterial(t, 3, []*commontypes.OrdererEndpoint{
		{ID: 0, Host: "orderer1.example.com", Port: 7050},
		{ID: 0, Host: "orderer2.example.com", Port: 7051},
	})
	retryProfile := &connection.RetryProfile{MaxElapsedTime: 30}
	tlsMaterials := connection.TLSMaterials{
		Mode:    connection.OneSideTLSMode,
		Cert:    []byte("fake-cert"),
		Key:     []byte("fake-key"),
		CACerts: [][]byte{[]byte("fake-ca-cert")},
	}
	params := ordererconn.MaterialParameters{
		TLS:   tlsMaterials,
		Retry: retryProfile,
		API:   commontypes.Deliver,
	}
	connMaterial := material.OrdererConnectionMaterial(params)
	require.Equal(t, retryProfile, connMaterial.Joint.Retry)
	for _, mat := range connMaterial.PerID {
		require.Equal(t, retryProfile, mat.Retry)
	}
	require.Equal(t, connection.OneSideTLSMode, connMaterial.Joint.TLS.Mode)
	for _, mat := range connMaterial.PerID {
		require.Equal(t, tlsMaterials.Mode, mat.TLS.Mode)
		require.Equal(t, tlsMaterials.Cert, mat.TLS.Cert)
		require.Equal(t, tlsMaterials.Key, mat.TLS.Key)
		require.Contains(t, mat.TLS.CACerts, tlsMaterials.CACerts[0])
	}
}

// Helper methods

func createConfigBlockPath(
	t *testing.T,
	channelID string,
	peerOrgCount uint32,
	endpoints []*commontypes.OrdererEndpoint,
) string {
	t.Helper()
	cryptoDir := t.TempDir()
	_, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir, &testcrypto.ConfigBlock{
		ChannelID:             channelID,
		PeerOrganizationCount: peerOrgCount,
		OrdererEndpoints:      endpoints,
	})
	require.NoError(t, err)
	return path.Join(cryptoDir, cryptogen.ConfigBlockFileName)
}

func createConfigBlockMaterial(
	t *testing.T,
	peerOrgCount uint32,
	endpoints []*commontypes.OrdererEndpoint,
) *ordererconn.ConfigBlockMaterial {
	t.Helper()
	blockPath := createConfigBlockPath(t, "test-channel", peerOrgCount, endpoints)
	material, err := ordererconn.LoadConfigBlockFromFile(blockPath)
	require.NoError(t, err)
	return material
}

func createBlockFile(t *testing.T, block *common.Block) string {
	t.Helper()
	blockPath := filepath.Join(t.TempDir(), "block.block")
	blockBytes, err := protoutil.Marshal(block)
	require.NoError(t, err)
	err = os.WriteFile(blockPath, blockBytes, 0o600)
	require.NoError(t, err)
	return blockPath
}
