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
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/test"
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
	client, err := deliver.New(&c.config.Orderer)
	if err != nil {
		return errors.Wrap(err, "failed to create orderer clients")
	}
	defer client.Close()

	dCtx, dCancel := context.WithCancel(ctx)
	defer dCancel()
	g, gCtx := errgroup.WithContext(dCtx)
	if c.config.SidecarEndpoint == nil || c.config.SidecarEndpoint.Empty() {
		g.Go(func() error {
			defer dCancel() // We stop sending if we can't track the received items.
			return runOrdererReceiver(gCtx, c.res, client)
		})
	} else {
		g.Go(func() error {
			defer dCancel() // We stop sending if we can't track the received items.
			return runSidecarReceiver(gCtx, &sidecarReceiverConfig{
				Endpoint: c.config.SidecarEndpoint,
				Res:      c.res,
			})
		})
	}

	for range c.config.BroadcastParallelism {
		g.Go(func() error {
			stream, streamErr := test.NewBroadcastStream(gCtx, &c.config.Orderer)
			if streamErr != nil {
				return errors.Wrap(streamErr, "failed to create a broadcast stream")
			}
			defer stream.Close()
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
