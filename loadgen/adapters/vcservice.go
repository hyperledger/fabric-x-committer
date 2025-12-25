/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
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
	commonConn, connErr := connection.NewLoadBalancedConnection(c.config)
	if connErr != nil {
		return errors.Wrapf(connErr, "failed to create connection to validator persisters")
	}
	defer connection.CloseConnectionsLog(commonConn)
	commonClient := servicepb.NewValidationAndCommitServiceClient(commonConn)
	_, setupError := commonClient.SetupSystemTablesAndNamespaces(ctx, nil)
	if setupError != nil {
		return errors.Wrap(setupError, "failed to setup system tables and namespaces")
	}
	if nextBlock, getErr := commonClient.GetNextBlockNumberToCommit(ctx, nil); getErr != nil {
		// We do not return error as we can proceed assuming no blocks were committed.
		logger.Infof("failed getting last committed block number: %v", getErr)
	} else if nextBlock != nil {
		c.nextBlockNum.Store(nextBlock.Number)
	} else {
		c.nextBlockNum.Store(0)
	}
	connections, connErr := connection.NewConnectionPerEndpoint(c.config)
	if connErr != nil {
		return connErr
	}
	defer connection.CloseConnectionsLog(connections...)

	streams := make([]servicepb.ValidationAndCommitService_StartValidateAndCommitStreamClient, 0, len(connections))
	for _, conn := range connections {
		client := servicepb.NewValidationAndCommitServiceClient(conn)
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
		stream := stream
		g.Go(func() error {
			return sendBlocks(ctx, &c.commonAdapter, txStream, workload.MapToVcBatch, stream.Send)
		})
		g.Go(func() error {
			defer dCancel() // We stop sending if we can't track the received items.
			return c.receiveStatus(gCtx, stream)
		})
	}
	return errors.Wrap(g.Wait(), "workload done")
}

func (c *VcAdapter) receiveStatus(
	ctx context.Context, stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamClient,
) error {
	for ctx.Err() == nil {
		responseBatch, err := stream.Recv()
		if err != nil {
			return errors.Wrap(connection.FilterStreamRPCError(err), "failed receiving response batch")
		}

		logger.Debugf("Received VC batch with %d items", len(responseBatch.Status))
		c.res.Metrics.OnReceiveBatch(toMetricsStatus(responseBatch.Status))
		if c.res.isReceiveLimit() {
			return nil
		}
	}
	return nil
}
