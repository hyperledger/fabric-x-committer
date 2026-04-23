/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/protoutil/identity"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

type (
	// Parameters defines the configuration for fault-tolerant block delivery from Orderer organizations.
	//
	// Fault Tolerance Level:
	//   - BFT (Byzantine Fault Tolerance): Verifies blocks and monitors for block withholding attacks
	//   - CFT (Crash Fault Tolerance): Verifies blocks only
	//   - NO (No Fault Tolerance): No verification (not for production use)
	//   - Empty: Defaults to BFT (highest fault tolerance)
	//
	// Processing State:
	//   - LastBlock: The most recently processed block (do NOT pass the genesis block here to allow the delivery to
	//	   deliver it for processing)
	//   - NextBlockVerificationConfig: Config block used to verify the next incoming block
	//   - LatestKnownConfig: The newest known config block (can be ahead of NextBlockVerificationConfig)
	//     It enables connectivity when NextBlockVerificationConfig has stale orderer
	//     endpoints/credentials. This occurs in two scenarios:
	//     1. A new organization joining long after genesis, where the starting config block has
	//        completely outdated endpoints. The caller provides current endpoints obtained out-of-band
	//        (e.g., via configuration file).
	//     2. Session resumption after all old orderer endpoints were replaced while the data stream
	//        hadn't yet processed the config block with the new endpoints.
	//
	// State Relationships:
	//   - If LatestKnownConfig is not provided, NextBlockVerificationConfig is used as the latest config.
	//   - If NextBlockVerificationConfig is not provided, policy verification starts only when a config
	//     block is delivered (LatestKnownConfig is NOT used for verification as it may not be relevant).
	//
	// Output Channels:
	//   - OutputBlock: Receives verified data blocks (without source information)
	//   - OutputBlockWithSourceID: Receives verified data blocks with source orderer ID
	//   - Note: At least one output channel must be provided
	//   - Note: Both channels can be provided simultaneously if needed
	//
	// Block Withholding Detection (BFT only):
	// The system uses parallel streams to detect malicious orderers withholding blocks:
	//   1. One stream receives full blocks (data stream)
	//   2. Multiple streams receive block headers only (header-only streams)
	//
	// Detection Process:
	//    - When a header arrives ahead of the full block, the system suspects the current data stream source
	//    - The system calculates the block gap (number of blocks the header stream is ahead)
	//    - If the data stream doesn't catch up within (SuspicionGracePeriodPerBlock * block_gap)
	//      the system switches to another orderer
	//    - After switching, the suspicion is cleared, allowing the new orderer time to establish
	//      connection and deliver blocks
	//
	// SuspicionGracePeriodPerBlock should account for:
	//  * Network latency between orderers and the client
	//  * Time to deliver a single full block (not the block generation rate)
	//  * Processing overhead for block validation
	//
	// Note: "Time to deliver a full block" refers to the delivery time of existing
	// blocks during catch-up, not the overall system's block generation capability.
	//
	// If SuspicionGracePeriodPerBlock is too short, honest orderers will be arbitrarily suspected
	// and the system will unnecessarily switch between orderers.
	Parameters struct {
		FaultToleranceLevel string
		TLS                 connection.TLSCredentials
		TLSCertHash         []byte
		Retry               *retry.Profile
		Signer              identity.SignerSerializer

		LatestKnownConfig           *common.Block
		NextBlockVerificationConfig *common.Block
		LastBlock                   *common.Block

		OutputBlock             chan<- *common.Block
		OutputBlockWithSourceID chan<- *deliver.BlockWithSourceID

		SuspicionGracePeriodPerBlock time.Duration

		// SkipBlockSignatureVerification mirrors ordererdial.Config.SkipBlockSignatureVerification.
		SkipBlockSignatureVerification bool
	}

	// SessionInfo represents the processing state of blocks to support recovery and resumption.
	// See Parameters for detailed information about these fields.
	SessionInfo struct {
		LatestKnownConfig           *common.Block
		NextBlockVerificationConfig *common.Block
		LastBlock                   *common.Block

		// SkipBlockSignatureVerification mirrors ordererdial.Config.SkipBlockSignatureVerification.
		SkipBlockSignatureVerification bool
	}
)

// DefaultSuspicionGracePeriodPerBlock is used when the parameters are not set by the user.
const DefaultSuspicionGracePeriodPerBlock = time.Second

// LoadParametersFromConfig returns orderer delivery parameters and channel-ID from a given config.
func LoadParametersFromConfig(c *ordererdial.Config) (p Parameters, err error) {
	tls, err := ordererdial.NewTLSCredentials(c.TLS)
	if err != nil {
		return p, err
	}
	var tlsCertHash []byte
	if tls.Mode == connection.MutualTLSMode {
		tlsCertHash, err = protoutil.HashTLSCertificate(tls.Cert)
		if err != nil {
			return p, err
		}
	}
	if c.LatestKnownConfigBlockPath == "" {
		return p, errors.New("latest known config block path is empty")
	}
	latestConfigBlock, err := protoutil.ReadBlockFromFile(c.LatestKnownConfigBlockPath)
	if err != nil {
		return p, errors.Wrap(err, "read config block")
	}
	signer, idErr := ordererdial.NewIdentitySigner(c.Identity)
	if idErr != nil {
		return p, errors.WithHint(idErr, "error creating identity signer")
	}
	return Parameters{
		FaultToleranceLevel:            c.FaultToleranceLevel,
		TLS:                            *tls,
		TLSCertHash:                    tlsCertHash,
		Retry:                          c.Retry,
		Signer:                         signer,
		SuspicionGracePeriodPerBlock:   c.SuspicionGracePeriodPerBlock,
		LatestKnownConfig:              latestConfigBlock,
		SkipBlockSignatureVerification: c.SkipBlockSignatureVerification,
	}, nil
}
