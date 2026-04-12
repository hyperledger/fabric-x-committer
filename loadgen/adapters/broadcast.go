/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"
	"maps"
	"slices"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
)

type (
	// Broadcaster can be either CFT or BFT broadcast.
	Broadcaster interface {
		Send(*common.Envelope) error
		SendBatch([]*common.Envelope) error
		Close() error
	}

	//nolint:containedctx // grpc's stream contain context.
	cftBroadcaster struct {
		ctx context.Context

		conn         *grpc.ClientConn
		client       ab.AtomicBroadcastClient
		streamCtx    context.Context
		streamCancel context.CancelFunc
		stream       ab.AtomicBroadcast_BroadcastClient
	}

	// bftBroadcaster supports broadcasting to multiple nodes (one per ID).
	// It should be used only for testing as it does not validate successful broadcast to a valid quorum.
	bftBroadcaster struct {
		idToStream map[uint32]*cftBroadcaster
	}
)

// NewBroadcastStream starts a new broadcast stream (non-production).
func NewBroadcastStream(ctx context.Context, config *ordererdial.Config) (Broadcaster, error) {
	tls, err := ordererdial.NewTLSCredentials(config.TLS)
	if err != nil {
		return nil, err
	}
	configMaterial, err := channelconfig.LoadConfigBlockMaterialFromFile(config.LatestKnownConfigBlockPath)
	if err != nil {
		return nil, err
	}
	m := ordererdial.NewDialInfo(configMaterial, ordererdial.Parameters{
		API:   commontypes.Broadcast,
		TLS:   *tls,
		Retry: config.Retry,
	})
	ftLevel, err := ordererdial.GetFaultToleranceLevel(config.FaultToleranceLevel)
	if err != nil {
		return nil, err
	}

	if ftLevel != ordererdial.BFT {
		return newCFTBroadcaster(ctx, m.Joint)
	}

	streams := make(map[uint32]*cftBroadcaster, len(m.PartyIDToDialInfo))
	for id, mPerID := range m.PartyIDToDialInfo {
		streams[id], err = newCFTBroadcaster(ctx, mPerID)
		if err != nil {
			return nil, errors.Join(err, connection.CloseConnections(slices.Collect(maps.Values(streams))...))
		}
	}
	return &bftBroadcaster{idToStream: streams}, nil
}

func newCFTBroadcaster(
	ctx context.Context, d *connection.DialInfo,
) (*cftBroadcaster, error) {
	conn, err := d.NewLoadBalancedConnection()
	if err != nil {
		return nil, err
	}
	return &cftBroadcaster{
		ctx:    ctx,
		conn:   conn,
		client: ab.NewAtomicBroadcastClient(conn),
	}, nil
}

// SendBatch sends a batch one by one.
func (s *bftBroadcaster) SendBatch(envelopes []*common.Envelope) error {
	return sendBatch(s, envelopes)
}

func (s *bftBroadcaster) Close() error {
	return connection.CloseConnections(slices.Collect(maps.Values(s.idToStream))...)
}

func (s *bftBroadcaster) Send(m *common.Envelope) error {
	errs := make([]error, 0, len(s.idToStream))
	for id, stream := range s.idToStream {
		err := stream.Send(m)
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "failed to send message to stream [%d]", id))
		}
	}
	return errors.Join(errs...)
}

// SendBatch sends a batch one by one.
func (s *cftBroadcaster) SendBatch(envelopes []*common.Envelope) error {
	return sendBatch(s, envelopes)
}

func (s *cftBroadcaster) Close() error {
	if s.streamCancel != nil {
		s.streamCancel()
	}
	return s.conn.Close()
}

func (s *cftBroadcaster) Send(m *common.Envelope) error {
	var err error
	var isUpdate bool
	// We try twice. Once with the existing stream and second time with a new one.
	for !isUpdate {
		isUpdate, err = s.update()
		if err != nil {
			return err
		}
		err = s.stream.Send(m)
		if err == nil {
			return nil
		}
		// Force update.
		s.streamCancel()
	}
	return errors.Wrap(err, "failed to send message")
}

// update returns error if there is no alive connection.
func (s *cftBroadcaster) update() (bool, error) {
	if s.stream != nil && s.streamCtx != nil && s.streamCtx.Err() == nil {
		return false, nil
	}
	if s.streamCancel != nil {
		// Ensure the previous stream context is canceled.
		s.streamCancel()
	}
	//nolint:fatcontext // The previous streamCtx is cancelled prior to this.
	s.streamCtx, s.streamCancel = context.WithCancel(s.ctx)

	var err error
	s.stream, err = s.client.Broadcast(s.streamCtx)
	if err != nil {
		s.streamCancel()
		return true, err
	}

	// We load the parameter on stack before starting the go routine.
	workerCtx := s.streamCtx
	workerStream := s.stream
	workerCancel := s.streamCancel
	go func() {
		defer workerCancel()
		for workerCtx.Err() == nil {
			_, streamErr := workerStream.Recv()
			if streamErr != nil {
				return
			}
		}
	}()
	return true, nil
}

// sendBatch sends a batch one by one.
func sendBatch(s Broadcaster, envelopes []*common.Envelope) error {
	for _, env := range envelopes {
		err := s.Send(env)
		if err != nil {
			return err
		}
	}
	return nil
}
