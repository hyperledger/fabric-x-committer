/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"

	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

// LoadParametersFromConfig returns orderer delivery parameters and channel-ID from a given config.
func LoadParametersFromConfig(c *ordererconn.Config) (*Parameters, error) {
	tls, err := ordererconn.NewTLSMaterials(c.TLS)
	if err != nil {
		return nil, err
	}
	if c.LastKnownConfigBlockPath == "" {
		return nil, errors.New("last known config block path is empty")
	}
	lastConfigBlock, err := configtxgen.ReadBlock(c.LastKnownConfigBlockPath)
	if err != nil {
		return nil, errors.Wrap(err, "read config block")
	}
	signer, idErr := ordererconn.NewIdentitySigner(c.Identity)
	if idErr != nil {
		return nil, errors.WithHint(idErr, "error creating identity signer")
	}
	return &Parameters{
		FaultToleranceLevel:     c.FaultToleranceLevel,
		TLS:                     *tls,
		Retry:                   c.Retry,
		Signer:                  signer,
		BlockWithholdingTimeout: c.BlockWithholdingTimeout,
		LivenessCheckInterval:   c.LivenessCheckInterval,
		SuspicionGracePeriod:    c.SuspicionGracePeriod,
		MaxBlocksAhead:          c.MaxBlocksAhead,
		Session: &SessionInfo{
			LastestKnownConfig: lastConfigBlock,
		},
	}, nil
}
