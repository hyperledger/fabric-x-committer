/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"fmt"
	"maps"
	"os"
	"path"
	"slices"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
)

// ConfigBlock represents the configuration of the config block.
type ConfigBlock struct {
	ChannelID                    string
	OrdererEndpoints             []*commontypes.OrdererEndpoint
	PeerOrganizationCount        uint32
	MetaNamespaceVerificationKey []byte
}

// CreateConfigTxFromConfigBlock creates a config TX.
func CreateConfigTxFromConfigBlock(block *common.Block) (*servicepb.LoadGenTx, error) {
	envelopeBytes := block.Data.Data[0]
	envelope, err := protoutil.GetEnvelopeFromBlock(envelopeBytes)
	if err != nil {
		return nil, errors.Wrap(err, "error getting envelope")
	}
	_, channelHdr, err := serialization.ParseEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	return &servicepb.LoadGenTx{
		Id: channelHdr.TxId,
		Tx: &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId: committerpb.ConfigNamespaceID,
				BlindWrites: []*applicationpb.Write{{
					Key:   []byte(committerpb.ConfigNamespaceID),
					Value: envelopeBytes,
				}},
			}},
		},
		EnvelopePayload:    envelope.Payload,
		EnvelopeSignature:  envelope.Signature,
		SerializedEnvelope: envelopeBytes,
	}, nil
}

// CreateConfigBlock creating a config block.
func CreateConfigBlock(policy *PolicyProfile) (*common.Block, error) {
	err := prepareCryptoMaterial(policy)
	if err != nil {
		return nil, err
	}

	configBlockPath := policy.ConfigBlockPath
	if configBlockPath == "" {
		configBlockPath = path.Join(policy.CryptoMaterialPath, "config-block.pb.bin")
	}

	block, err := configtxgen.ReadBlock(configBlockPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed reading config block from %s", policy.ConfigBlockPath)
	}
	return block, nil
}

// prepareCryptoMaterial generates the crypto material for a policy if it wasn't generated before.
func prepareCryptoMaterial(policy *PolicyProfile) error {
	if policy.CryptoMaterialPath == "" {
		tempDir, err := makeTemporaryDir()
		if err != nil {
			return err
		}
		policy.CryptoMaterialPath = tempDir
	}
	err := os.MkdirAll(policy.CryptoMaterialPath, 0o750)
	if err != nil {
		return errors.Wrap(err, "error creating crypto material folder")
	}

	configBlockPath := path.Join(policy.CryptoMaterialPath, "config-block.pb.bin")
	if _, fErr := os.Stat(configBlockPath); fErr == nil {
		return nil
	}

	_, metaPolicy := newPolicyEndorser(policy.CryptoMaterialPath, policy.NamespacePolicies[committerpb.MetaNamespaceID])
	_, err = CreateDefaultConfigBlockWithCrypto(policy.CryptoMaterialPath, &ConfigBlock{
		MetaNamespaceVerificationKey: metaPolicy.GetThresholdRule().GetPublicKey(),
		OrdererEndpoints:             policy.OrdererEndpoints,
		ChannelID:                    policy.ChannelID,
		PeerOrganizationCount:        policy.PeerOrganizationCount,
	})
	return err
}

// CreateDefaultConfigBlock creates a config block with default values.
func CreateDefaultConfigBlock(conf *ConfigBlock) (*common.Block, error) {
	target, err := makeTemporaryDir()
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = os.RemoveAll(target)
	}()
	return CreateDefaultConfigBlockWithCrypto(target, conf)
}

func makeTemporaryDir() (string, error) {
	tempDir, err := os.MkdirTemp("", "sc-loadgen-crypto-*")
	if err != nil {
		return "", errors.Wrap(err, "error creating temp dir for crypto-material")
	}
	return tempDir, nil
}

// CreateDefaultConfigBlockWithCrypto creates a config block with crypto material.
func CreateDefaultConfigBlockWithCrypto(targetPath string, conf *ConfigBlock) (*common.Block, error) {
	orgs := make([]cryptogen.OrganizationParameters, 0, int(conf.PeerOrganizationCount)+len(conf.OrdererEndpoints))

	ordererOrgsMap := make(map[uint32][]cryptogen.OrdererEndpoint)
	for _, e := range conf.OrdererEndpoints {
		ordererOrgsMap[e.ID] = append(ordererOrgsMap[e.ID], cryptogen.OrdererEndpoint{
			Address: e.Address(),
			API:     e.API,
		})
	}
	// We clear the IDs, and let the cryptogen tool to re-assign IDs to the orderer endpoints.
	ordererOrgs := slices.Collect(maps.Values(ordererOrgsMap))

	if len(ordererOrgs) == 0 {
		// We need at least one orderer org to create a config block.
		ordererOrgs = append(ordererOrgs, []cryptogen.OrdererEndpoint{{Address: "localhost:7050"}})
	}

	for i, endpoints := range ordererOrgs {
		orgs = append(orgs, cryptogen.OrganizationParameters{
			Name:             fmt.Sprintf("orderer-org-%d", i),
			Domain:           fmt.Sprintf("orderer-org-%d.com", i),
			OrdererEndpoints: endpoints,
			ConsenterNodes: []cryptogen.Node{{
				CommonName: fmt.Sprintf("consenter-org-%d", i),
				Hostname:   fmt.Sprintf("consenter-org-%d.com", i),
			}},
		})
	}

	for i := range conf.PeerOrganizationCount {
		orgs = append(orgs, cryptogen.OrganizationParameters{
			Name:   fmt.Sprintf("peer-org-%d", i),
			Domain: fmt.Sprintf("peer-org-%d.com", i),
			PeerNodes: []cryptogen.Node{{
				CommonName: fmt.Sprintf("sidecar-peer-org-%d", i),
				Hostname:   fmt.Sprintf("sidecar-peer-org-%d.com", i),
			}},
		})
	}

	metaKey := conf.MetaNamespaceVerificationKey
	if len(metaKey) == 0 {
		// We must supply a valid meta namespace key.
		_, metaKey = sigtest.NewKeyPair(signature.Ecdsa)
	}

	// We create the config block on the basis of the default Fabric X config profile.
	// It will use the default parameters defined in this profile (e.g., Policies, OrdererType, etc.).
	// The generated organizations will use the default parameters taken from the first orderer org defined
	// in the profile.
	return cryptogen.CreateDefaultConfigBlockWithCrypto(cryptogen.ConfigBlockParameters{
		TargetPath:                   targetPath,
		BaseProfile:                  configtxgen.SampleFabricX,
		ChannelID:                    conf.ChannelID,
		Organizations:                orgs,
		MetaNamespaceVerificationKey: metaKey,
	})
}
