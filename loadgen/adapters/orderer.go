/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/broadcastdeliver"
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
	client, err := broadcastdeliver.New(&c.config.Orderer)
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
				ChannelID: c.config.Orderer.ChannelID,
				Endpoint:  c.config.SidecarEndpoint,
				Res:       c.res,
			})
		})
	}

	for range c.config.BroadcastParallelism {
		g.Go(func() error {
			stream, err := client.Broadcast(gCtx)
			if err != nil {
				return errors.Wrap(err, "failed to create a broadcast stream")
			}
			return sendBlocks(
				gCtx, &c.commonAdapter, txStream,
				func(txs []*protoblocktx.Tx) ([]*common.Envelope, []string, error) {
					return mapToBatch(txs, stream)
				},
				func(envs []*common.Envelope) error {
					return sendBatch(envs, stream)
				},
			)
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

// mapToBatch creates a batch of orderer envelopes. It uses the envelope header ID to track the TXs latency.
func mapToBatch(
	txs []*protoblocktx.Tx, stream *broadcastdeliver.EnvelopedStream,
) ([]*common.Envelope, []string, error) {
	envs := make([]*common.Envelope, len(txs))
	txIDs := make([]string, len(txs))
	for i, tx := range txs {
		var err error
		envs[i], txIDs[i], err = stream.CreateEnvelope(tx)
		if err != nil {
			return nil, nil, err
		}
	}
	return envs, txIDs, nil
}

// sendBatch sends a batch one by one.
func sendBatch(envelopes []*common.Envelope, stream *broadcastdeliver.EnvelopedStream) error {
	for _, env := range envelopes {
		err := stream.Send(env)
		if err != nil {
			return err
		}
	}
	return nil
}
