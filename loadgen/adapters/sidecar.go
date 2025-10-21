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

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type (
	// SidecarAdapter applies load on the sidecar.
	SidecarAdapter struct {
		commonAdapter
		config *SidecarClientConfig
	}
)

// NewSidecarAdapter instantiate SidecarAdapter.
func NewSidecarAdapter(config *SidecarClientConfig, res *ClientResources) *SidecarAdapter {
	return &SidecarAdapter{
		commonAdapter: commonAdapter{res: res},
		config:        config,
	}
}

// RunWorkload applies load on the sidecar.
func (c *SidecarAdapter) RunWorkload(ctx context.Context, txStream *workload.StreamWithSetup) error {
	if len(c.config.OrdererServers) == 0 {
		return errors.New("no orderer servers configured")
	}
	orderer, err := mock.NewMockOrderer(&mock.OrdererConfig{
		ServerConfigs: c.config.OrdererServers,
	})
	if err != nil {
		return err
	}

	dCtx, dCancel := context.WithCancel(ctx)
	defer dCancel()
	g, gCtx := errgroup.WithContext(dCtx)

	g.Go(func() error {
		return connection.StartService(gCtx, orderer, c.config.OrdererServers...)
	})

	// The sidecar adapter submits a config block manually.
	policy := *c.res.Profile.Transaction.Policy
	policy.OrdererEndpoints = ordererconn.NewEndpoints(0, "msp", c.config.OrdererServers...)
	configBlock, err := workload.CreateConfigBlock(&policy)
	if err != nil {
		return err
	}
	if err = orderer.SubmitBlock(ctx, configBlock); err != nil {
		return err
	}
	c.nextBlockNum.Add(1)

	g.Go(func() error {
		defer dCancel() // We stop sending if we can't track the received items.
		return runSidecarReceiver(gCtx, &sidecarReceiverParameters{
			ClientConfig: c.config.SidecarClient,
			Res:          c.res,
		})
	})
	g.Go(func() error {
		return sendBlocks(gCtx, &c.commonAdapter, txStream, workload.MapToOrdererBlock,
			func(fabricBlock *common.Block) error {
				return orderer.SubmitBlock(gCtx, fabricBlock)
			},
		)
	})
	return errors.Wrap(g.Wait(), "workload done")
}

// Supports specify which phases an adapter supports.
// The sidecar does not support config transactions as it filters them.
// To generate a config TX, the orderer must submit a config block.
func (*SidecarAdapter) Supports() Phases {
	return Phases{
		Config:     false,
		Namespaces: true,
		Load:       true,
	}
}
