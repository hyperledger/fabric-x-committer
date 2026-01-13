/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type (
	// BroadcastStream implements broadcast, with the addition of automatically signing the envelope.
	BroadcastStream struct {
		Broadcaster
		ConnectionManager *ordererconn.ConnectionManager
	}

	// Broadcaster can be either CFT or BFT broadcast.
	Broadcaster interface {
		Send(*common.Envelope) error
	}

	//nolint:containedctx // grpc's stream contain context.
	cftBroadcaster struct {
		ctx               context.Context
		connectionManager *ordererconn.ConnectionManager

		configVersion uint64
		conn          *grpc.ClientConn
		streamCtx     context.Context
		streamCancel  context.CancelFunc
		stream        ab.AtomicBroadcast_BroadcastClient
	}

	// bftBroadcaster supports broadcasting to multiple nodes (one per ID).
	// It should be used only for testing as it does not validate successful broadcast to a valid quorum.
	//
	//nolint:containedctx // grpc's stream contain context.
	bftBroadcaster struct {
		ctx               context.Context
		connectionManager *ordererconn.ConnectionManager

		configVersion uint64
		streamCtx     context.Context
		streamCancel  context.CancelFunc
		idToStream    map[uint32]*cftBroadcaster
	}
)

// NewBroadcastStream starts a new broadcast stream (non-production).
func NewBroadcastStream(ctx context.Context, config *ordererconn.Config) (*BroadcastStream, error) {
	if err := ordererconn.ValidateConsensusType(config); err != nil {
		return nil, err
	}

	cm, err := ordererconn.NewConnectionManager(config)
	if err != nil {
		return nil, err
	}

	stream := &BroadcastStream{ConnectionManager: cm}
	switch config.ConsensusType {
	case ordererconn.Cft:
		stream.Broadcaster = &cftBroadcaster{
			ctx:               ctx,
			connectionManager: cm,
		}
	case ordererconn.Bft:
		stream.Broadcaster = &bftBroadcaster{
			ctx:               ctx,
			connectionManager: cm,
		}
	default:
		return nil, errors.Newf("invalid consensus type: '%s'", config.ConsensusType)
	}
	return stream, nil
}

// CloseConnections closes all the connections for the stream.
func (s *BroadcastStream) CloseConnections() {
	s.ConnectionManager.CloseConnections()
}

// Close is included to implement the [io.Closer] interface.
func (s *BroadcastStream) Close() error {
	s.CloseConnections()
	return nil
}

// SendBatch sends a batch one by one.
func (s *BroadcastStream) SendBatch(envelopes []*common.Envelope) error {
	for _, env := range envelopes {
		err := s.Send(env)
		if err != nil {
			return err
		}
	}
	return nil
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
	if s.conn != nil && s.stream != nil && s.streamCtx != nil && s.streamCtx.Err() == nil {
		return false, nil
	}
	if s.streamCancel != nil {
		// Ensure the previous stream context is cancelled.
		s.streamCancel()
	}
	//nolint:fatcontext // The previous streamCtx is cancelled prior to this.
	s.streamCtx, s.streamCancel = context.WithCancel(s.ctx)

	// We also support using this module without a connection manager.
	if s.connectionManager != nil {
		isStale := s.connectionManager.IsStale(s.configVersion)
		if isStale {
			s.conn, s.configVersion = s.connectionManager.GetConnection(ordererconn.WithAPI(ordererconn.Broadcast))
		}
	}

	if s.conn == nil {
		return false, ordererconn.ErrNoConnections
	}
	var err error
	s.stream, err = ab.NewAtomicBroadcastClient(s.conn).Broadcast(s.streamCtx)
	if err != nil {
		return true, err
	}

	// We load the parameter on stack before starting the go routine.
	workerCtx := s.streamCtx
	workerStream := s.stream
	workerCancel := s.streamCancel
	go func() {
		defer workerCancel()
		responseCollector(workerCtx, workerStream)
	}()
	return true, nil
}

func responseCollector(ctx context.Context, stream ab.AtomicBroadcast_BroadcastClient) {
	for ctx.Err() == nil {
		_, streamErr := stream.Recv()
		if streamErr != nil {
			return
		}
	}
}

func (s *bftBroadcaster) Send(m *common.Envelope) error {
	s.update()
	if len(s.idToStream) == 0 {
		return ordererconn.ErrNoConnections
	}
	errs := make([]error, 0, len(s.idToStream))
	for id, stream := range s.idToStream {
		err := stream.Send(m)
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "failed to send message to stream [%d]", id))
		}
	}
	return errors.Join(errs...)
}

func (s *bftBroadcaster) update() {
	// We also support using this module without a connection manager.
	if s.connectionManager == nil {
		return
	}

	isStale := s.connectionManager.IsStale(s.configVersion)
	if !isStale {
		return
	}
	if s.streamCancel != nil {
		// Ensure the previous stream context is cancelled.
		s.streamCancel()
	}
	//nolint:fatcontext // The previous streamCtx is cancelled prior to this.
	s.streamCtx, s.streamCancel = context.WithCancel(s.ctx)

	idToConn, configVersion := s.connectionManager.GetConnectionPerID(ordererconn.WithAPI(ordererconn.Broadcast))
	s.configVersion = configVersion

	streams := make(map[uint32]*cftBroadcaster)
	for id, conn := range idToConn {
		streams[id] = &cftBroadcaster{
			ctx:  s.streamCtx,
			conn: conn,
		}
	}
	s.idToStream = streams
}
