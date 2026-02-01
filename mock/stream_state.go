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

// streamStateManager is a helper struct to be embedded in the mock services.
// It helps manage the open streams, and detect the number of active stream, and
// to which endpoint each client connected to.
type (
	streamStateManager[T any] struct {
		streamMap        map[uint64]*streamState[T]
		streamsMu        sync.Mutex
		streamsIDCounter atomic.Uint64
	}
	streamState[T any] struct {
		index          uint64
		serverEndpoint string
		clientEndpoint string
		internalState  *T
	}
)

func (s *streamStateManager[T]) allocateStream(streamCtx context.Context, internalState *T) *streamState[T] {
	state := &streamState[T]{
		index:          s.streamsIDCounter.Add(1),
		serverEndpoint: utils.ExtractServerAddress(streamCtx),
		clientEndpoint: commonutils.ExtractRemoteAddress(streamCtx),
		internalState:  internalState,
	}

	s.streamsMu.Lock()
	if s.streamMap == nil {
		s.streamMap = make(map[uint64]*streamState[T])
	}
	s.streamMap[state.index] = state
	s.streamsMu.Unlock()

	logger.Infof("Allocated stream [%d] on server %s for client %s",
		state.index, state.serverEndpoint, state.clientEndpoint)
	return state
}

func (s *streamStateManager[T]) releaseStream(state *streamState[T]) {
	logger.Infof("Removing stream [%d] on server [%s]", state.index, state.serverEndpoint)
	s.streamsMu.Lock()
	if s.streamMap != nil {
		delete(s.streamMap, state.index)
	}
	s.streamsMu.Unlock()
}

// Streams returns the current active streams in the orderer they were created.
func (s *streamStateManager[T]) Streams() []*T {
	streams := s.allInternalStreams()
	ret := make([]*T, len(streams))
	for i, stream := range streams {
		ret[i] = stream.internalState
	}
	return ret
}

// StreamsByEndpoints returns the current active streams that was accessed using one of the specific endpoints,
// in the orderer they were created.
func (s *streamStateManager[T]) StreamsByEndpoints(endpoints ...string) []*T {
	streams := s.allInternalStreams()
	ret := make([]*T, 0, len(streams))
	for _, stream := range streams {
		if slices.Contains(endpoints, stream.serverEndpoint) {
			ret = append(ret, stream.internalState)
		}
	}
	return ret
}

func (s *streamStateManager[T]) allInternalStreams() []*streamState[T] {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.streamMap == nil {
		return nil
	}
	streams := slices.Collect(maps.Values(s.streamMap))
	slices.SortFunc(streams, func(a, b *streamState[T]) int {
		return int(a.index) - int(b.index) //nolint:gosec // required for sorting.
	})
	return streams
}
