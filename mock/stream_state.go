/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"cmp"
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
	streamStateManager[T any] struct {
		indexToStreamState map[uint64]*internalStreamState[T]
		streamsMu          sync.Mutex
		streamsIDCounter   atomic.Uint64
	}

	internalStreamState[T any] struct {
		info  StreamInfo
		state *T
	}

	// StreamInfo holds the information of a mock stream.
	StreamInfo struct {
		Index          uint64
		ServerEndpoint string
		ClientEndpoint string
	}
)

// StreamsStates returns the current active streams in the orderer they were created.
func (s *streamStateManager[T]) StreamsStates() []*T {
	streams := s.internalStreams()
	states := make([]*T, len(streams))
	for i, stream := range streams {
		states[i] = stream.state
	}
	return states
}

// StreamsStatesByServerEndpoints returns the current active streams that was accessed using
// one of the specific endpoints, in the orderer they were created.
func (s *streamStateManager[T]) StreamsStatesByServerEndpoints(endpoints ...string) []*T {
	streams := s.internalStreams()
	states := make([]*T, 0, len(streams))
	for _, stream := range streams {
		if slices.Contains(endpoints, stream.info.ServerEndpoint) {
			states = append(states, stream.state)
		}
	}
	return states
}

func (s *streamStateManager[T]) internalStreams() []*internalStreamState[T] {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.indexToStreamState == nil {
		return nil
	}
	streams := slices.Collect(maps.Values(s.indexToStreamState))
	slices.SortFunc(streams, func(a, b *internalStreamState[T]) int {
		return cmp.Compare(a.info.Index, b.info.Index)
	})
	return streams
}

func (s *streamStateManager[T]) registerStream(streamCtx context.Context, factory func(StreamInfo) *T) *T {
	info := StreamInfo{
		Index:          s.streamsIDCounter.Add(1),
		ServerEndpoint: utils.ExtractServerAddress(streamCtx),
		ClientEndpoint: commonutils.ExtractRemoteAddress(streamCtx),
	}
	context.AfterFunc(streamCtx, func() {
		logger.Infof("Closing stream [%d]", info.Index)
		s.streamsMu.Lock()
		defer s.streamsMu.Unlock()
		delete(s.indexToStreamState, info.Index)
	})

	logger.Infof("Registering stream [%d] on server %s for client %s",
		info.Index, info.ServerEndpoint, info.ClientEndpoint)

	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.indexToStreamState == nil {
		s.indexToStreamState = make(map[uint64]*internalStreamState[T])
	}
	state := factory(info)
	s.indexToStreamState[info.Index] = &internalStreamState[T]{info: info, state: state}
	return state
}
