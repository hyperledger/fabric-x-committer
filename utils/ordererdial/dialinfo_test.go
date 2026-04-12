/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererdial_test

import (
	"maps"
	"path"
	"slices"
	"testing"

	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

func TestOrdererConnectionMaterial(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                       string
		endpoints                  []*commontypes.OrdererEndpoint
		params                     ordererdial.Parameters
		expectedJointEndpointCount int
		expectedIDs                []uint32
		expectedEndpointCountPerID int
		expectCACerts              bool
	}{
		{
			name:                       "single orderer organization",
			endpoints:                  nil,
			params:                     ordererdial.Parameters{API: commontypes.Deliver},
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
			params:                     ordererdial.Parameters{API: commontypes.Deliver},
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
			params:                     ordererdial.Parameters{API: commontypes.Deliver},
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
			params:                     ordererdial.Parameters{API: commontypes.Broadcast},
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
			params:                     ordererdial.Parameters{API: commontypes.Deliver},
			expectedJointEndpointCount: 2,
			expectedEndpointCountPerID: 2,
			expectedIDs:                []uint32{0},
		},
		{
			name:      "TLS mode server includes CA certs",
			endpoints: nil,
			params: ordererdial.Parameters{
				TLS: connection.TLSCredentials{Mode: connection.OneSideTLSMode},
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
			params: ordererdial.Parameters{
				TLS: connection.TLSCredentials{Mode: connection.MutualTLSMode},
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
			dialInfo := ordererdial.NewDialInfo(material, tc.params)
			require.NotNil(t, dialInfo)
			require.Len(t, dialInfo.Joint.Endpoints, tc.expectedJointEndpointCount)
			require.ElementsMatch(t, tc.expectedIDs, slices.Collect(maps.Keys(dialInfo.PartyIDToDialInfo)))
			for _, di := range dialInfo.PartyIDToDialInfo {
				require.Len(t, di.Endpoints, tc.expectedEndpointCountPerID)
			}
			if tc.expectCACerts {
				require.NotEmpty(t, dialInfo.Joint.TLS.CACerts)
			} else {
				require.Empty(t, dialInfo.Joint.TLS.CACerts)
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

	params := ordererdial.Parameters{
		TLS: connection.TLSCredentials{Mode: connection.NoneTLSMode},
		API: commontypes.Deliver,
	}

	jointOrders := make(map[string]any)
	perIDOrders := make(map[uint32]map[string]any)
	for i := range uint32(3) {
		perIDOrders[i] = make(map[string]any)
	}
	firstCall := ordererdial.NewDialInfo(material, params)
	for range 10 {
		dialInfo := ordererdial.NewDialInfo(material, params)
		require.ElementsMatch(t, firstCall.Joint.Endpoints, dialInfo.Joint.Endpoints)

		jointOrders[connection.AddressString(dialInfo.Joint.Endpoints...)] = nil
		for id, di := range dialInfo.PartyIDToDialInfo {
			require.ElementsMatch(t, firstCall.PartyIDToDialInfo[id].Endpoints, di.Endpoints)
			perIDOrders[id][connection.AddressString(di.Endpoints...)] = nil
		}
	}

	require.Greater(t, len(jointOrders), 1, "endpoints should be shuffled")
	for id, o := range perIDOrders {
		require.Greaterf(t, len(o), 1, "endpoints for ID %d should be shuffled", id)
	}
}

func TestParameterPropagation(t *testing.T) {
	t.Parallel()

	material := createConfigBlockMaterial(t, 3, []*commontypes.OrdererEndpoint{
		{ID: 0, Host: "orderer1.example.com", Port: 7050},
		{ID: 0, Host: "orderer2.example.com", Port: 7051},
	})
	retryProfile := &retry.Profile{MaxElapsedTime: 30}
	tlsCreds := connection.TLSCredentials{
		Mode:    connection.OneSideTLSMode,
		Cert:    []byte("fake-cert"),
		Key:     []byte("fake-key"),
		CACerts: [][]byte{[]byte("fake-ca-cert")},
	}
	params := ordererdial.Parameters{
		TLS:   tlsCreds,
		Retry: retryProfile,
		API:   commontypes.Deliver,
	}
	dialInfo := ordererdial.NewDialInfo(material, params)
	require.Equal(t, retryProfile, dialInfo.Joint.Retry)
	for _, di := range dialInfo.PartyIDToDialInfo {
		require.Equal(t, retryProfile, di.Retry)
	}
	require.Equal(t, connection.OneSideTLSMode, dialInfo.Joint.TLS.Mode)
	for _, di := range dialInfo.PartyIDToDialInfo {
		require.Equal(t, tlsCreds.Mode, di.TLS.Mode)
		require.Equal(t, tlsCreds.Cert, di.TLS.Cert)
		require.Equal(t, tlsCreds.Key, di.TLS.Key)
		require.Contains(t, di.TLS.CACerts, tlsCreds.CACerts[0])
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
) *channelconfig.ConfigBlockMaterial {
	t.Helper()
	blockPath := createConfigBlockPath(t, "test-channel", peerOrgCount, endpoints)
	material, err := channelconfig.LoadConfigBlockMaterialFromFile(blockPath)
	require.NoError(t, err)
	return material
}
