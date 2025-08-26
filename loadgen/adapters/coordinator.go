/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// CoordinatorAdapter applies load on the coordinator.
	CoordinatorAdapter struct {
		commonAdapter
		config *connection.ClientConfig
	}
)

// NewCoordinatorAdapter instantiate CoordinatorAdapter.
func NewCoordinatorAdapter(config *connection.ClientConfig, res *ClientResources) *CoordinatorAdapter {
	return &CoordinatorAdapter{
		commonAdapter: commonAdapter{res: res},
		config:        config,
	}
}

// RunWorkload applies load on the coordinator.
func (c *CoordinatorAdapter) RunWorkload(ctx context.Context, txStream *workload.StreamWithSetup) error {
	coordinatorDialConfig, err := connection.NewSingleDialConfig(c.config)
	if err != nil {
		return errors.Wrapf(err, "failed creating coordinator dial config")
	}
	// connecting to the coordinator.
	conn, connErr := connection.Connect(coordinatorDialConfig)
	if connErr != nil {
		return errors.Wrapf(err, "failed to connect to coordinator at %s", c.config.Endpoint.Address())
	}
	defer connection.CloseConnectionsLog(conn)
	client := protocoordinatorservice.NewCoordinatorClient(conn)
	if lastCommittedBlock, getErr := client.GetLastCommittedBlockNumber(ctx, nil); getErr != nil {
		// We do not return error as we can proceed assuming no blocks were committed.
		logger.Infof("cannot fetch the last committed block number: %v", getErr)
	} else if lastCommittedBlock.Block != nil {
		c.nextBlockNum.Store(lastCommittedBlock.Block.Number + 1)
	} else {
		c.nextBlockNum.Store(0)
	}

	logger.Info("Opening stream")
	stream, err := client.BlockProcessing(ctx)
	if err != nil {
		return errors.Wrap(err, "failed creating stream to coordinator")
	}

	dCtx, dCancel := context.WithCancel(ctx)
	defer dCancel()
	g, gCtx := errgroup.WithContext(dCtx)
	g.Go(func() error {
		return sendBlocks(gCtx, &c.commonAdapter, txStream, workload.MapToCoordinatorBatch, stream.Send)
	})
	g.Go(func() error {
		defer dCancel() // We stop sending if we can't track the received items.
		return c.receiveStatus(gCtx, stream)
	})
	return errors.Wrap(g.Wait(), "workload done")
}

// Progress a submitted block indicates progress for the coordinator as it guaranteed to preserve the order.
func (c *CoordinatorAdapter) Progress() uint64 {
	return c.nextBlockNum.Load()
}

func (c *CoordinatorAdapter) receiveStatus(
	ctx context.Context, stream protocoordinatorservice.Coordinator_BlockProcessingClient,
) error {
	for ctx.Err() == nil {
		txStatus, err := stream.Recv()
		if err != nil {
			return errors.Wrap(connection.FilterStreamRPCError(err), "failed receiving block status")
		}

		logger.Debugf("Received coordinator status batch with %d items", len(txStatus.Status))
		statusBatch := make([]metrics.TxStatus, 0, len(txStatus.Status))
		for id, status := range txStatus.Status {
			statusBatch = append(statusBatch, metrics.TxStatus{TxID: id, Status: status.Code})
		}
		c.res.Metrics.OnReceiveBatch(statusBatch)
		if c.res.isReceiveLimit() {
			return nil
		}
	}
	return nil
}
