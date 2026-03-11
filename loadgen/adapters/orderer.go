/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// OrdererAdapter applies load on the sidecar.
	OrdererAdapter struct {
		commonAdapter
		config *OrdererClientConfig
	}
)

// NewOrdererAdapter instantiate OrdererAdapter.
func NewOrdererAdapter(config *OrdererClientConfig, res *ClientResources) *OrdererAdapter {
	return &OrdererAdapter{
		commonAdapter: commonAdapter{res: res},
		config:        config,
	}
}

// RunWorkload applies load on the sidecar.
func (c *OrdererAdapter) RunWorkload(ctx context.Context, txStream *workload.StreamWithSetup) error {
	dCtx, dCancel := context.WithCancel(ctx)
	defer dCancel()
	g, gCtx := errgroup.WithContext(dCtx)
	if (c.config.SidecarClient == nil) ||
		(c.config.SidecarClient.Endpoint == nil) ||
		(c.config.SidecarClient.Endpoint.Empty()) {
		g.Go(func() error {
			defer dCancel() // We stop sending if we can't track the received items.
			return runOrdererReceiver(gCtx, c.res, &c.config.Orderer)
		})
	} else {
		g.Go(func() error {
			defer dCancel() // We stop sending if we can't track the received items.
			return runSidecarReceiver(gCtx, &sidecarReceiverParameters{
				ClientConfig: c.config.SidecarClient,
				Res:          c.res,
			})
		})
	}

	streams := make([]*BroadcastStream, c.config.BroadcastParallelism)
	for i := range streams {
		var err error
		streams[i], err = NewBroadcastStream(gCtx, &c.config.Orderer)
		if err != nil {
			connection.CloseConnectionsLog(streams[:i]...)
			return err
		}
	}
	defer connection.CloseConnectionsLog(streams...)

	logger.Infof("Starting sending blocks with %d streams", len(streams))
	for _, stream := range streams {
		g.Go(func() error {
			return sendBlocks(gCtx, &c.commonAdapter, txStream, workload.MapToEnvelopeBatch, stream.SendBatch)
		})
	}
	return errors.Wrap(g.Wait(), "workload done")
}

// Supports specify which phases an adapter supports.
// The sidecar does not support config transactions as it filters them.
// To generate a config TX, the orderer must submit a config block.
func (*OrdererAdapter) Supports() Phases {
	return Phases{
		Config:     false,
		Namespaces: true,
		Load:       true,
	}
}
