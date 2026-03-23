/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"os"
	"path"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

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

// CreateOrLoadConfigBlockWithCrypto generates the crypto artifacts for a policy.
// If it is already generated, it will load the config block from the file system.
func CreateOrLoadConfigBlockWithCrypto(policy *PolicyProfile) (*common.Block, error) {
	if policy.ArtifactsPath != "" {
		configBlockPath := path.Join(policy.ArtifactsPath, cryptogen.ConfigBlockFileName)
		if _, fErr := os.Stat(configBlockPath); fErr == nil {
			block, err := protoutil.ReadBlockFromFile(configBlockPath)
			return block, errors.Wrapf(err, "failed reading config block from %s", configBlockPath)
		}
	}
	return CreateOrExtendConfigBlockWithCrypto(policy)
}

// CreateOrExtendConfigBlockWithCrypto generates or extends the crypto artifacts for a policy.
// This will generate a new config block, or overwrite the existing config block if it already exists.
func CreateOrExtendConfigBlockWithCrypto(policy *PolicyProfile) (*common.Block, error) {
	if policy.ArtifactsPath == "" {
		tempDir, err := os.MkdirTemp("", "sc-loadgen-artifacts-*")
		if err != nil {
			return nil, errors.Wrap(err, "error creating temp dir for the artifacts")
		}
		policy.ArtifactsPath = tempDir
	}
	return testcrypto.CreateOrExtendConfigBlockWithCrypto(policy.ArtifactsPath, &testcrypto.ConfigBlock{
		OrdererEndpoints:      policy.OrdererEndpoints,
		ChannelID:             policy.ChannelID,
		PeerOrganizationCount: policy.PeerOrganizationCount,
	})
}
