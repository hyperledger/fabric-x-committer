/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"fmt"
	"maps"
	"os"
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

// CreateConfigTx creating a config TX.
func CreateConfigTx(policy *PolicyProfile) (*servicepb.LoadGenTx, error) {
	envelopeBytes, err := CreateConfigEnvelope(policy)
	if err != nil {
		return nil, err
	}
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

	txEndorser := NewTxEndorserVerifier(policy)
	metaPolicy := txEndorser.Policy(committerpb.MetaNamespaceID)
	return CreateDefaultConfigBlock(&ConfigBlock{
		MetaNamespaceVerificationKey: metaPolicy.VerificationPolicy().GetThresholdRule().GetPublicKey(),
		OrdererEndpoints:             policy.OrdererEndpoints,
		ChannelID:                    policy.ChannelID,
	}, configtxgen.TwoOrgsSampleFabricX)
}

// CreateDefaultConfigBlock creates a config block with default values.
func CreateDefaultConfigBlock(conf *ConfigBlock, profileName string) (*common.Block, error) {
	target, err := os.MkdirTemp("", "cryptogen-temp-*")
	if err != nil {
		return nil, errors.Wrap(err, "failed creating temp dir for config block generation")
	}
	defer func() {
		_ = os.RemoveAll(target)
	}()
	return CreateDefaultConfigBlockWithCrypto(target, conf, profileName)
}

// CreateDefaultConfigBlockWithCrypto creates a config block with crypto material.
func CreateDefaultConfigBlockWithCrypto(
	targetPath string, conf *ConfigBlock, profileName string,
) (*common.Block, error) {
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

	return cryptogen.CreateDefaultConfigBlockWithCrypto(cryptogen.ConfigBlockParameters{
		TargetPath:                   targetPath,
		BaseProfile:                  profileName,
		ChannelID:                    conf.ChannelID,
		Organizations:                orgs,
		MetaNamespaceVerificationKey: metaKey,
	})
}
