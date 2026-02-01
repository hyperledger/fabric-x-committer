/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	commonutils "github.com/hyperledger/fabric-x-common/common/util"

	"github.com/hyperledger/fabric-x-committer/utils"
)

type (
	// streamStateManager is a helper struct to be embedded in the mock services.
	// It helps manage the open streams, detect the number of active stream, and
	// to which endpoint each client connected to.
	// In addition, it allows configuring a shared, persistent state between multiple streams
	// according to the stream's server endpoint.
	// This allows uniform behavior for each mock server.
	streamStateManager[T, S any] struct {
		streamIndexMap                   map[uint64]*internalStreamState[T]
		streamEndpointPersistentStateMap map[string]*S
		streamsMu                        sync.Mutex
		streamsIDCounter                 atomic.Uint64
	}

	// StreamInfo holds the information of a mock stream.
	StreamInfo struct {
		Index          uint64
		ServerEndpoint string
		ClientEndpoint string
	}

	internalStreamState[T any] struct {
		info  StreamInfo
		state *T
	}
)

// Streams returns the current active streams in the orderer they were created.
func (s *streamStateManager[T, S]) Streams() []*T {
	streams := s.internalStreams()
	states := make([]*T, len(streams))
	for i, stream := range streams {
		states[i] = stream.state
	}
	return states
}

// StreamsByEndpoints returns the current active streams that was accessed using one of the specific endpoints,
// in the orderer they were created.
func (s *streamStateManager[T, S]) StreamsByEndpoints(endpoints ...string) []*T {
	streams := s.internalStreams()
	states := make([]*T, 0, len(streams))
	for _, stream := range streams {
		if slices.Contains(endpoints, stream.info.ServerEndpoint) {
			states = append(states, stream.state)
		}
	}
	return states
}

func (s *streamStateManager[T, S]) RegisterSharedState(address string, sharedState *S) {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.streamEndpointPersistentStateMap == nil {
		s.streamEndpointPersistentStateMap = make(map[string]*S)
	}
	s.streamEndpointPersistentStateMap[address] = sharedState
}

func (s *streamStateManager[T, S]) internalStreams() []*internalStreamState[T] {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.streamIndexMap == nil {
		return nil
	}
	streams := slices.Collect(maps.Values(s.streamIndexMap))
	slices.SortFunc(streams, func(a, b *internalStreamState[T]) int {
		return int(a.info.Index) - int(b.info.Index) //nolint:gosec // required for sorting.
	})
	return streams
}

func (s *streamStateManager[T, S]) registerStream(streamCtx context.Context, factory func(StreamInfo, *S) *T) *T {
	info := StreamInfo{
		Index:          s.streamsIDCounter.Add(1),
		ServerEndpoint: utils.ExtractServerAddress(streamCtx),
		ClientEndpoint: commonutils.ExtractRemoteAddress(streamCtx),
	}
	context.AfterFunc(streamCtx, func() {
		logger.Infof("Closing stream [%d]", info.Index)
		s.streamsMu.Lock()
		defer s.streamsMu.Unlock()
		delete(s.streamIndexMap, info.Index)
	})

	logger.Infof("Registering stream [%d] on server %s for client %s",
		info.Index, info.ServerEndpoint, info.ClientEndpoint)

	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.streamIndexMap == nil {
		s.streamIndexMap = make(map[uint64]*internalStreamState[T])
	}
	if s.streamEndpointPersistentStateMap == nil {
		s.streamEndpointPersistentStateMap = make(map[string]*S)
	}
	sharedState, loaded := s.streamEndpointPersistentStateMap[info.ServerEndpoint]
	if !loaded {
		sharedState = new(S)
		s.streamEndpointPersistentStateMap[info.ServerEndpoint] = sharedState
	}
	state := factory(info, sharedState)
	s.streamIndexMap[info.Index] = &internalStreamState[T]{info: info, state: state}
	return state
}
