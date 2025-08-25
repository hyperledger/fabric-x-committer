/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protovcservice"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// VcAdapter applies load on the VC.
	VcAdapter struct {
		commonAdapter
		config *connection.MultiClientConfig
	}
)

// NewVCAdapter instantiate VcAdapter.
func NewVCAdapter(config *connection.MultiClientConfig, res *ClientResources) *VcAdapter {
	return &VcAdapter{
		commonAdapter: commonAdapter{res: res},
		config:        config,
	}
}

// RunWorkload applies load on the VC.
func (c *VcAdapter) RunWorkload(ctx context.Context, txStream *workload.StreamWithSetup) error {
	commonDial, dialErr := connection.NewLoadBalancedDialConfig(*c.config)
	if dialErr != nil {
		return errors.Wrapf(dialErr, "could not create dial config for vcs")
	}
	commonConn, connErr := connection.Connect(commonDial)
	if connErr != nil {
		return errors.Wrapf(connErr, "failed to create connection to validator persisters")
	}
	commonClient := protovcservice.NewValidationAndCommitServiceClient(commonConn)
	_, setupError := commonClient.SetupSystemTablesAndNamespaces(ctx, nil)
	if setupError != nil {
		return errors.Wrap(setupError, "failed to setup system tables and namespaces")
	}
	if lastCommittedBlock, getErr := commonClient.GetLastCommittedBlockNumber(ctx, nil); getErr != nil {
		// We do not return error as we can proceed assuming no blocks were committed.
		logger.Infof("failed getting last committed block number: %v", getErr)
	} else if lastCommittedBlock.Block != nil {
		c.nextBlockNum.Store(lastCommittedBlock.Block.Number + 1)
	} else {
		c.nextBlockNum.Store(0)
	}
	connections, connErr := connection.OpenConnections(*c.config)
	if connErr != nil {
		return errors.Wrap(connErr, "failed opening connection to vc-service")
	}
	defer connection.CloseConnectionsLog(connections...)

	streams := make([]protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamClient, 0, len(connections))
	for _, conn := range connections {
		client := protovcservice.NewValidationAndCommitServiceClient(conn)
		logger.Info("Opening VC stream")
		stream, streamErr := client.StartValidateAndCommitStream(ctx)
		if streamErr != nil {
			return errors.Wrapf(streamErr, "failed opening stream to %s", conn.Target())
		}
		streams = append(streams, stream)
	}

	dCtx, dCancel := context.WithCancel(ctx)
	defer dCancel()
	g, gCtx := errgroup.WithContext(dCtx)
	for _, stream := range streams {
		g.Go(func() error {
			return sendBlocks(ctx, &c.commonAdapter, txStream, c.mapToBatch, stream.Send)
		})
		g.Go(func() error {
			defer dCancel() // We stop sending if we can't track the received items.
			return c.receiveStatus(gCtx, stream)
		})
	}
	return errors.Wrap(g.Wait(), "workload done")
}

func (c *VcAdapter) receiveStatus(
	ctx context.Context, stream protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamClient,
) error {
	for ctx.Err() == nil {
		responseBatch, err := stream.Recv()
		if err != nil {
			return errors.Wrap(connection.FilterStreamRPCError(err), "failed receiving response batch")
		}

		logger.Debugf("Received VC batch with %d items", len(responseBatch.Status))

		statusBatch := make([]metrics.TxStatus, 0, len(responseBatch.Status))
		for id, status := range responseBatch.Status {
			statusBatch = append(statusBatch, metrics.TxStatus{TxID: id, Status: status.Code})
		}
		c.res.Metrics.OnReceiveBatch(statusBatch)
		if c.res.isReceiveLimit() {
			return nil
		}
	}
	return nil
}

// mapToBatch creates a VC batch. It uses the protoblocktx.Tx.Id to track the TXs latency.
func (c *VcAdapter) mapToBatch(txs []*protoblocktx.Tx) (*protovcservice.TransactionBatch, []string, error) {
	batchTxs := make([]*protovcservice.Transaction, len(txs))
	for i, tx := range txs {
		batchTxs[i] = &protovcservice.Transaction{
			ID:          tx.Id,
			Namespaces:  tx.Namespaces,
			BlockNumber: c.NextBlockNum(),
			TxNum:       uint32(i), //nolint:gosec // int -> uint32.
		}
	}
	return &protovcservice.TransactionBatch{Transactions: batchTxs}, getTXsIDs(txs), nil
}
