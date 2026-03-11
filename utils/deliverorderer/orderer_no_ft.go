/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"context"
	"crypto/sha256"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/types"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

// NoFTParameters needed for deliver to run.
type NoFTParameters struct {
	ClientConfig *ordererconn.Config
	NextBlockNum uint64
	OutputBlock  chan<- *common.Block
}

// ToQueueWithNoFT connects to an orderer delivery server and delivers the stream to a queue (go channel).
// It provides a simple, no verification method, to load blocks the an orderer.
// It should not be used in production.
// It returns when an error occurs or when the context is done.
// It will attempt to reconnect on errors.
func ToQueueWithNoFT(ctx context.Context, noFtParams NoFTParameters) error {
	params, err := LoadParametersFromConfig(noFtParams.ClientConfig)
	if err != nil {
		return err
	}
	configMaterial, err := ordererconn.LoadConfigBlock(params.Session.LastestKnownConfig)
	if configMaterial == nil || err != nil {
		return err
	}
	tlsCertHash := sha256.Sum256(params.TLS.Cert)

	m := configMaterial.OrdererConnectionMaterial(ordererconn.MaterialParameters{
		API:   types.Deliver,
		TLS:   params.TLS,
		Retry: params.Retry,
	})
	conn, connErr := m.Joint.NewLoadBalancedConnection()
	if connErr != nil {
		return connErr
	}
	defer connection.CloseConnectionsLog(conn)
	return deliver.ToQueue(ctx, deliver.Parameters{
		StreamCreator: ordererStreamCreator(conn),
		ChannelID:     configMaterial.ChannelID,
		Signer:        params.Signer,
		TLSCertHash:   tlsCertHash[:],
		OutputBlock:   noFtParams.OutputBlock,
		NextBlockNum:  noFtParams.NextBlockNum,
		HeaderOnly:    false,
	})
}
