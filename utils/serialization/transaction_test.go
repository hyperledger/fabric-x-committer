/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serialization_test

import (
	"encoding/pem"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

const localhost = "localhost"

func TestExtractAppTLSCAsFromEnvelope(t *testing.T) {
	t.Parallel()
	targetPath := t.TempDir()

	block, err := cryptogen.CreateOrExtendConfigBlockWithCrypto(cryptogen.ConfigBlockParameters{
		TargetPath:  targetPath,
		BaseProfile: configtxgen.SampleFabricX,
		ChannelID:   "test-channel",
		Organizations: []cryptogen.OrganizationParameters{
			{
				Name:   "orderer-org",
				Domain: "orderer-org.com",
				OrdererEndpoints: []*types.OrdererEndpoint{
					{Host: localhost, Port: 7050},
				},
				ConsenterNodes: []cryptogen.Node{
					{CommonName: "consenter", Hostname: "consenter.com"},
				},
				OrdererNodes: []cryptogen.Node{
					{CommonName: "orderer", Hostname: "orderer.com", SANS: []string{localhost}},
				},
			},
			{
				Name:   "peer-org",
				Domain: "peer-org.com",
				PeerNodes: []cryptogen.Node{
					{CommonName: "peer0", Hostname: "peer0.com", SANS: []string{localhost}},
				},
			},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, block.Data.Data)

	t.Run("extracts TLS CAs from valid config envelope", func(t *testing.T) {
		t.Parallel()
		certs, err := serialization.ExtractAppTLSCAsFromEnvelope(block.Data.Data[0])
		require.NoError(t, err)
		require.NotEmpty(t, certs, "should extract at least one TLS CA certificate")

		for _, cert := range certs {
			require.NotEmpty(t, cert)
			blk, _ := pem.Decode(cert)
			require.NotNil(t, blk, "each cert should be valid PEM")
		}
	})

	t.Run("returns error for invalid envelope", func(t *testing.T) {
		t.Parallel()
		_, err := serialization.ExtractAppTLSCAsFromEnvelope([]byte("invalid"))
		require.Error(t, err)
	})

	t.Run("returns error for nil envelope", func(t *testing.T) {
		t.Parallel()
		_, err := serialization.ExtractAppTLSCAsFromEnvelope(nil)
		require.Error(t, err)
	})
}
