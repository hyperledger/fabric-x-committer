/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"math/rand/v2"
	"os"
	"path/filepath"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/core/config/configtest"
	"github.com/hyperledger/fabric-x-common/internaltools/configtxgen"
	"github.com/hyperledger/fabric-x-common/internaltools/configtxgen/genesisconfig"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// ConfigBlock represents the configuration of the config block.
type ConfigBlock struct {
	ChannelID                    string
	OrdererEndpoints             []*connection.OrdererEndpoint
	MetaNamespaceVerificationKey []byte
}

// CreateConfigTx creating a config TX.
func CreateConfigTx(policy *PolicyProfile) (*protoblocktx.Tx, error) {
	envelopeBytes, err := CreateConfigEnvelope(policy)
	if err != nil {
		return nil, err
	}
	return &protoblocktx.Tx{
		Id: "config tx",
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId: types.ConfigNamespaceID,
			BlindWrites: []*protoblocktx.Write{{
				Key:   []byte(types.ConfigNamespaceID),
				Value: envelopeBytes,
			}},
		}},
		Signatures: make([][]byte, 1),
	}, nil
}

// CreateConfigEnvelope creating a meta policy.
func CreateConfigEnvelope(policy *PolicyProfile) ([]byte, error) {
	block, err := CreateConfigBlock(policy)
	if err != nil {
		return nil, err
	}
	return block.Data.Data[0], nil
}

// CreateConfigBlock creating a config block.
func CreateConfigBlock(policy *PolicyProfile) (*common.Block, error) {
	if policy.ConfigBlockPath != "" {
		block, err := configtxgen.ReadBlock(policy.ConfigBlockPath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed reading config block from %s", policy.ConfigBlockPath)
		}
		return block, nil
	}

	txSigner := NewTxSignerVerifier(policy)
	policyNamespaceSigner, ok := txSigner.HashSigners[types.MetaNamespaceID]
	if !ok {
		return nil, errors.New("no policy namespace signer found; cannot create namespaces")
	}
	return CreateDefaultConfigBlock(&ConfigBlock{
		MetaNamespaceVerificationKey: policyNamespaceSigner.pubKey,
		OrdererEndpoints:             policy.OrdererEndpoints,
	})
}

// CreateDefaultConfigBlock creates a config block with default values.
func CreateDefaultConfigBlock(conf *ConfigBlock) (*common.Block, error) {
	configBlock := genesisconfig.Load(genesisconfig.SampleFabricX, configtest.GetDevConfigDir())
	tlsCertPath := filepath.Join(configtest.GetDevConfigDir(), "msp", "tlscacerts", "tlsroot.pem")
	for _, consenter := range configBlock.Orderer.ConsenterMapping {
		consenter.Identity = tlsCertPath
		consenter.ClientTLSCert = tlsCertPath
		consenter.ServerTLSCert = tlsCertPath
	}
	// Resetting Arma.Path to an empty string as it isn't needed.
	configBlock.Orderer.Arma.Path = ""

	if conf.MetaNamespaceVerificationKey == nil {
		conf.MetaNamespaceVerificationKey, _ = NewHashSignerVerifier(&Policy{
			Scheme: signature.Ecdsa,
			Seed:   rand.Int64(),
		}).GetVerificationKeyAndSigner()
	}
	metaPubKeyPath, err := writeTempPem(conf.MetaNamespaceVerificationKey)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp PEM file")
	}
	defer func() {
		_ = os.Remove(metaPubKeyPath)
	}()
	configBlock.Application.MetaNamespaceVerificationKeyPath = metaPubKeyPath

	if len(conf.OrdererEndpoints) > 0 {
		if len(configBlock.Orderer.Organizations) < 1 {
			return nil, errors.New("no organizations configured")
		}
		sourceOrg := *configBlock.Orderer.Organizations[0]
		configBlock.Orderer.Organizations = nil

		orgMap := make(map[string]*[]string)
		for _, e := range conf.OrdererEndpoints {
			orgEndpoints, ok := orgMap[e.MspID]
			if !ok {
				org := sourceOrg
				org.ID = e.MspID
				org.Name = e.MspID
				org.OrdererEndpoints = nil
				configBlock.Orderer.Organizations = append(configBlock.Orderer.Organizations, &org)
				orgMap[e.MspID] = &org.OrdererEndpoints
				orgEndpoints = &org.OrdererEndpoints
			}
			*orgEndpoints = append(*orgEndpoints, e.String())
		}
	}

	channelID := conf.ChannelID
	if channelID == "" {
		channelID = "chan"
	}
	block, err := configtxgen.GetOutputBlock(configBlock, channelID)
	return block, errors.Wrap(err, "failed to get output block")
}

func writeTempPem(data []byte) (string, error) {
	tempPem, err := os.CreateTemp("", "*.pem")
	if err != nil {
		return "", errors.Wrap(err, "failed to create temp PEM file")
	}
	defer func() {
		_ = tempPem.Close()
	}()
	_, err = tempPem.Write(data)
	return tempPem.Name(), errors.Wrap(err, "failed to write temp PEM file")
}
