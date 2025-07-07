/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
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
		return errors.Wrap(err, "failed to create orderer")
	}

	dCtx, dCancel := context.WithCancel(ctx)
	defer dCancel()
	g, gCtx := errgroup.WithContext(dCtx)

	g.Go(func() error {
		return connection.StartService(gCtx, orderer, nil, nil)
	})
	for _, conf := range c.config.OrdererServers {
		conf := conf
		g.Go(func() error {
			return connection.RunGrpcServerMainWithError(gCtx, conf, func(s *grpc.Server) {
				ab.RegisterAtomicBroadcastServer(s, orderer)
			})
		})
	}

	// The sidecar adapter submits a config block manually.
	policy := *c.res.Profile.Transaction.Policy
	policy.OrdererEndpoints = connection.NewOrdererEndpoints(0, "msp", c.config.OrdererServers...)
	configBlock, err := workload.CreateConfigBlock(&policy)
	if err != nil {
		return errors.Wrap(err, "failed to create config block")
	}
	orderer.SubmitBlock(ctx, configBlock)
	c.nextBlockNum.Add(1)

	g.Go(func() error {
		defer dCancel() // We stop sending if we can't track the received items.
		return runSidecarReceiver(gCtx, &sidecarReceiverConfig{
			ChannelID: c.config.ChannelID,
			Endpoint:  c.config.SidecarEndpoint,
			Res:       c.res,
		})
	})
	g.Go(func() error {
		return sendBlocks(gCtx, &c.commonAdapter, txStream, c.mapToBlock,
			func(fabricBlock *common.Block) error {
				if !orderer.SubmitBlock(gCtx, fabricBlock) {
					return errors.New("failed to submit block")
				}
				return nil
			})
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

// mapToBlock creates a Fabric block. It uses the envelope's TX ID to track the TXs latency.
func (c *SidecarAdapter) mapToBlock(txs []*protoblocktx.Tx) (*common.Block, []string, error) {
	data := make([][]byte, len(txs))
	txIDs := make([]string, len(txs))
	for i, tx := range txs {
		env, txID, err := serialization.CreateEnvelope(c.config.ChannelID, nil, tx)
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed creating envelope")
		}
		txIDs[i] = txID
		data[i], err = proto.Marshal(env)
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed marshaling envelope")
		}
	}
	return &common.Block{
		Header: &common.BlockHeader{
			Number: c.NextBlockNum(),
		},
		Data: &common.BlockData{
			Data: data,
		},
	}, txIDs, nil
}
