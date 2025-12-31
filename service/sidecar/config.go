/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type (
	// Config holds the configuration of the sidecar service. This includes
	// sidecar endpoint, committer endpoint to which the sidecar pushes the block and pulls statuses,
	// and the config of ledger service, and the orderer setup.
	// It may contain the orderer endpoint from which the sidecar pulls blocks.
	Config struct {
		Server                        *connection.ServerConfig  `mapstructure:"server"`
		Monitoring                    monitoring.Config         `mapstructure:"monitoring"`
		Committer                     *connection.ClientConfig  `mapstructure:"committer"`
		Orderer                       ordererconn.Config        `mapstructure:"orderer"`
		Ledger                        LedgerConfig              `mapstructure:"ledger"`
		Notification                  NotificationServiceConfig `mapstructure:"notification"`
		LastCommittedBlockSetInterval time.Duration             `mapstructure:"last-committed-block-set-interval"`
		WaitingTxsLimit               int                       `mapstructure:"waiting-txs-limit"`
		// ChannelBufferSize is the buffer size that will be used to queue blocks, requests, and statuses.
		ChannelBufferSize int       `mapstructure:"channel-buffer-size"`
		Bootstrap         Bootstrap `mapstructure:"bootstrap"`
	}
	// Bootstrap configures how to obtain the bootstrap configuration.
	Bootstrap struct {
		// GenesisBlockFilePath is the path for the genesis block.
		// If omitted, the local configuration will be used.
		GenesisBlockFilePath string `mapstructure:"genesis-block-file-path" yaml:"genesis-block-file-path,omitempty"`
	}

	// LedgerConfig holds the ledger path.
	LedgerConfig struct {
		Path string `mapstructure:"path"`
	}

	// NotificationServiceConfig holds the parameters for notifications.
	NotificationServiceConfig struct {
		// MaxTimeout is an upper limit on the request's timeout to prevent resource exhaustion.
		// If a request doesn't specify a timeout, this value will be used.
		MaxTimeout time.Duration `mapstructure:"max-timeout"`
	}
)

const (
	defaultNotificationMaxTimeout = time.Minute
	defaultBufferSize             = 100
)

// LoadOrganizationsFromGenesisBlock loads the genesis-block given bootstrap config.
func LoadOrganizationsFromGenesisBlock(bootstrap Bootstrap) ([]*ordererconn.OrganizationMaterial, error) {
	if bootstrap.GenesisBlockFilePath == "" {
		return nil, nil
	}
	configBlock, err := configtxgen.ReadBlock(bootstrap.GenesisBlockFilePath)
	if err != nil {
		return nil, errors.Wrap(err, "read config block")
	}
	return GetOrganizationsFromConfigBlock(configBlock)
}

// GetOrganizationsFromConfigBlock retrieve the organization materials from a config block.
func GetOrganizationsFromConfigBlock(configBlock *common.Block) ([]*ordererconn.OrganizationMaterial, error) {
	envelope, err := protoutil.ExtractEnvelope(configBlock, 0)
	if err != nil {
		return nil, errors.Wrap(err, "failed to extract envelope")
	}
	return GetOrganizationsFromEnvelope(envelope)
}

// GetOrganizationsFromEnvelope retrieve the organization materials from a config transaction.
// For now, it fetches the following:
// - Orderer endpoints.
// - RootCAs per organization.
func GetOrganizationsFromEnvelope(envelope *common.Envelope) ([]*ordererconn.OrganizationMaterial, error) {
	bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
	if err != nil {
		return nil, errors.Wrap(err, "failed to create config bundle")
	}
	orgsMaterial, err := readOrganizationsFromBundle(bundle)
	if err != nil {
		return nil, err
	}
	return orgsMaterial, nil
}

func readOrganizationsFromBundle(bundle *channelconfig.Bundle) ([]*ordererconn.OrganizationMaterial, error) {
	ordererCfg, ok := bundle.OrdererConfig()
	if !ok {
		return nil, errors.New("could not find orderer config")
	}
	organizationMaterials := make([]*ordererconn.OrganizationMaterial, 0, len(ordererCfg.Organizations()))
	for orgID, org := range ordererCfg.Organizations() {
		var endpoints []*commontypes.OrdererEndpoint
		endpointsStr := org.Endpoints()
		for _, eStr := range endpointsStr {
			e, err := commontypes.ParseOrdererEndpoint(eStr)
			if err != nil {
				return nil, err
			}
			e.MspID = orgID
			endpoints = append(endpoints, e)
		}
		organizationMaterials = append(organizationMaterials, &ordererconn.OrganizationMaterial{
			MspID:     orgID,
			Endpoints: endpoints,
			CACerts:   org.MSP().GetTLSRootCerts(),
		})
	}
	return organizationMaterials, nil
}
