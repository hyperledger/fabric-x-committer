/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// SvAdapter applies load on the SV.
	SvAdapter struct {
		commonAdapter
		config *connection.MultiClientConfig
	}
)

// NewSVAdapter instantiate SvAdapter.
func NewSVAdapter(config *connection.MultiClientConfig, res *ClientResources) *SvAdapter {
	return &SvAdapter{
		commonAdapter: commonAdapter{res: res},
		config:        config,
	}
}

// RunWorkload applies load on the SV.
func (c *SvAdapter) RunWorkload(ctx context.Context, txStream *workload.StreamWithSetup) error {
	updateMsg, err := createUpdate(c.res.Profile.Transaction.Policy)
	if err != nil {
		return err
	}
	connections, err := connection.NewConnectionPerEndpoint(c.config)
	if err != nil {
		return err
	}
	defer connection.CloseConnectionsLog(connections...)

	dCtx, dCancel := context.WithCancel(ctx)
	defer dCancel()
	g, gCtx := errgroup.WithContext(dCtx)
	streams := make([]protosigverifierservice.Verifier_StartStreamClient, len(connections))
	for i, conn := range connections {
		client := protosigverifierservice.NewVerifierClient(conn)
		logger.Infof("Opening stream to %s", c.config.Endpoints[i])
		streams[i], err = client.StartStream(gCtx)
		if err != nil {
			return errors.Wrapf(err, "failed opening a stream to %s", c.config.Endpoints[i])
		}

		logger.Infof("Set verification verification policy")
		err = streams[i].Send(&protosigverifierservice.Batch{Update: updateMsg})
		if err != nil {
			return errors.Wrap(err, "failed submitting verification policy")
		}
	}

	for _, stream := range streams {
		stream := stream
		g.Go(func() error {
			return sendBlocks(gCtx, &c.commonAdapter, txStream, workload.MapToVerifierBatch, stream.Send)
		})
		g.Go(func() error {
			defer dCancel() // We stop sending if we can't track the received items.
			return c.receiveStatus(gCtx, stream)
		})
	}
	return errors.Wrap(g.Wait(), "workload done")
}

func createUpdate(policy *workload.PolicyProfile) (*protosigverifierservice.Update, error) {
	txSigner := workload.NewTxSignerVerifier(policy)

	envelopeBytes, err := workload.CreateConfigEnvelope(policy)
	if err != nil {
		return nil, err
	}
	updateMsg := &protosigverifierservice.Update{
		Config: &protoblocktx.ConfigTransaction{
			Envelope: envelopeBytes,
		},
		NamespacePolicies: &protoblocktx.NamespacePolicies{
			Policies: make([]*protoblocktx.PolicyItem, 0, len(txSigner.HashSigners)),
		},
	}

	for ns, p := range txSigner.HashSigners {
		if ns == types.MetaNamespaceID {
			continue
		}
		policy, err := proto.Marshal(p.GetVerificationPolicy())
		if err != nil {
			return nil, errors.Wrap(err, "failed to serialize policy")
		}
		updateMsg.NamespacePolicies.Policies = append(
			updateMsg.NamespacePolicies.Policies,
			&protoblocktx.PolicyItem{
				Namespace: ns,
				Policy:    policy,
			},
		)
	}

	return updateMsg, nil
}

func (c *SvAdapter) receiveStatus(
	ctx context.Context, stream protosigverifierservice.Verifier_StartStreamClient,
) error {
	for ctx.Err() == nil {
		responseBatch, err := stream.Recv()
		if err != nil {
			return errors.Wrap(connection.FilterStreamRPCError(err), "failed receiving verification status")
		}

		logger.Debugf("Received SV batch with %d responses", len(responseBatch.Responses))
		statusBatch := make([]metrics.TxStatus, len(responseBatch.Responses))
		for i, response := range responseBatch.Responses {
			logger.Debugf("Received Responses: %s", response.Status)
			statusBatch[i] = metrics.TxStatus{TxID: response.Ref.TxId, Status: response.Status}
		}
		c.res.Metrics.OnReceiveBatch(statusBatch)
		if c.res.isReceiveLimit() {
			return nil
		}
	}
	return nil
}
