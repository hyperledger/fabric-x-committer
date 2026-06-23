/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

type (
	// notifier uses a single go-routine for processing.
	// Each stream has its own notificationQueue (stream-level).
	// All requests flow into the global requestQueue and are accumulated in subscriptions (txID -> requests).
	// Status updates from statusQueue are dispatched by grouping responses per stream and
	// writing to each stream's notificationQueue.
	// Deadlock is prevented by buffered channels and single-threaded processing in run().
	notifier struct {
		committerpb.UnimplementedNotifierServer
		bufferSize         int
		maxTimeout         time.Duration
		maxActiveTxIDs     int
		maxTxIDsPerRequest int
		streamWriteTimeout time.Duration
		metrics            *perfMetrics
		// requestQueue receives requests from users.
		requestQueue chan *notificationRequest

		// StreamAllTransactions support
		allTxStreams   []*allTxStream
		allTxStreamsMu sync.RWMutex
	}

	notificationRequest struct {
		request *committerpb.NotificationRequest
		// streamEventQueue is used to submit notifications to the users (includes the request's context).
		streamEventQueue channel.Writer[*committerpb.NotificationResponse]

		// Internal use. Used to keep track of the request, and release its resources when it is fulfilled.
		timer   *time.Timer
		pending int
	}

	// subscriptions maps from TX ID to a set of notificationRequest objects
	// and tracks available slots for new subscriptions.
	subscriptions struct {
		txIDToRequests     map[string]map[*notificationRequest]any
		availableSlots     int
		streamWriteTimeout time.Duration
	}

	// committedBlockWithTxs contains the essential data from a committed block
	// for streaming to StreamAllTransactions clients. This is a clean interface
	// that separates the notifier from relay's internal blockWithStatus structure.
	committedBlockWithTxs struct {
		blockNumber uint64
		txs         []*servicepb.TxWithRef
		statuses    []committerpb.Status
	}

	// allTxStream represents a single StreamAllTransactions client subscription.
	// Each stream has its own filters and receives committed blocks via a channel.
	allTxStream struct {
		// Filters (empty = no filter)
		filterNamespaces []string
		filterStatuses   []committerpb.Status

		// Content flags
		includeReadWriteSets bool
		includeEndorsements  bool
		includeMetadata      bool

		// Communication
		blockQueue chan *committedBlockWithTxs

		// ctx is stored in the struct to manage the stream's lifecycle.
		// This is intentional and required for detecting stream cancellation.
		//nolint:containedctx // Context needed for stream lifecycle management
		ctx    context.Context
		cancel context.CancelFunc
	}
)

func newNotifier(bufferSize int, conf *NotificationServiceConfig, metrics *perfMetrics) *notifier {
	return &notifier{
		bufferSize:         bufferSize,
		maxTimeout:         conf.MaxTimeout,
		maxActiveTxIDs:     conf.MaxActiveTxIDs,
		maxTxIDsPerRequest: conf.MaxTxIDsPerRequest,
		streamWriteTimeout: conf.StreamWriteTimeout,
		metrics:            metrics,
		requestQueue:       make(chan *notificationRequest, bufferSize),
	}
}

func (n *notifier) run(
	ctx context.Context,
	statusQueue chan []*committerpb.TxStatus,
	committedBlockWithTxs chan *committedBlockWithTxs,
) error {
	notifyMap := subscriptions{
		txIDToRequests:     make(map[string]map[*notificationRequest]any),
		availableSlots:     n.maxActiveTxIDs,
		streamWriteTimeout: n.streamWriteTimeout,
	}
	timeoutQueue := make(chan *notificationRequest, cap(n.requestQueue))
	timeoutQueueCtx := channel.NewWriter(ctx, timeoutQueue)

	reject := func(req *notificationRequest, txIDs []string, reason string) {
		req.streamEventQueue.Write(&committerpb.NotificationResponse{
			RejectedTxIds: &committerpb.RejectedTxIds{
				TxIds:  txIDs,
				Reason: reason,
			},
		})
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req := <-n.requestQueue:
			txIDs := req.request.TxStatusRequest.GetTxIds()
			// Per-request limit: reject the entire request, similar to query service limits
			// (e.g., max keys per query). The client controls request size and should respect it.
			if len(txIDs) > n.maxTxIDsPerRequest {
				reject(req, txIDs, fmt.Sprintf(
					"request contains %d tx IDs, max allowed is %d", len(txIDs), n.maxTxIDsPerRequest,
				))
				continue
			}
			// Global active limit: accept what fits, reject the rest. Unlike the per-request limit,
			// the global limit depends on factors outside the client's control (other streams,
			// pending subscriptions), so partial acceptance is more appropriate.
			rejected, uniqueNew := notifyMap.addRequest(req)
			if len(rejected) > 0 {
				reject(req, rejected, "active tx IDs limit reached")
			}
			promutil.AddToGauge(n.metrics.notifierPendingTxIDs, req.pending)
			promutil.AddToGauge(n.metrics.notifierUniquePendingTxIDs, uniqueNew)
			if req.pending > 0 {
				req.timer = time.AfterFunc(req.request.Timeout.AsDuration(), func() {
					timeoutQueueCtx.Write(req)
				})
			}
		case status := <-statusQueue:
			pendingTxIDsRemoved, uniquePendingTxIDsRemoved := notifyMap.removeAndEnqueueStatusEvents(status)
			n.recordRemovals(pendingTxIDsRemoved, uniquePendingTxIDsRemoved)
			promutil.AddToCounter(n.metrics.notifierTxIDsStatusDeliveries, pendingTxIDsRemoved)
		case req := <-timeoutQueue:
			pendingTxIDsRemoved, uniquePendingTxIDsRemoved := notifyMap.removeAndEnqueueTimeoutEvents(req)
			n.recordRemovals(pendingTxIDsRemoved, uniquePendingTxIDsRemoved)
			promutil.AddToCounter(n.metrics.notifierTxIDsTimeoutDeliveries, pendingTxIDsRemoved)
		case block := <-committedBlockWithTxs:
			n.dispatchBlockToAllTxStreams(ctx, block)
		}
	}
}

func (n *notifier) recordRemovals(pendingTxIDsRemoved, uniquePendingTxIDsRemoved int) {
	promutil.AddToGauge(n.metrics.notifierPendingTxIDs, -pendingTxIDsRemoved)
	promutil.AddToGauge(n.metrics.notifierUniquePendingTxIDs, -uniquePendingTxIDsRemoved)
}

// OpenNotificationStream implements the [protonotify.NotifierServer] API.
func (n *notifier) OpenNotificationStream(stream committerpb.Notifier_OpenNotificationStreamServer) error {
	n.metrics.notifierActiveStreams.Inc()
	defer n.metrics.notifierActiveStreams.Dec()

	g, gCtx := errgroup.WithContext(stream.Context())
	requestQueue := channel.NewWriter(gCtx, n.requestQueue)
	streamEventQueue := channel.Make[*committerpb.NotificationResponse](gCtx, n.bufferSize)

	g.Go(func() error {
		for gCtx.Err() == nil {
			req, err := stream.Recv()
			if err != nil {
				return errors.Wrap(err, "error receiving request")
			}
			fixTimeout(req, n.maxTimeout)
			requestQueue.Write(&notificationRequest{
				request:          req,
				streamEventQueue: streamEventQueue,
			})
		}
		return gCtx.Err()
	})
	g.Go(func() error {
		for gCtx.Err() == nil {
			res, ok := streamEventQueue.Read()
			if !ok {
				break
			}
			if err := stream.Send(res); err != nil {
				return err
			}
		}
		return gCtx.Err()
	})
	return wrapNotifierError(g.Wait())
}

// StreamAllTransactions implements the [committerpb.NotifierServer] API.
// It streams all committed transactions to the client, with optional filtering
// by namespace and transaction status.
func (n *notifier) StreamAllTransactions(
	req *committerpb.StreamAllRequest,
	stream committerpb.Notifier_StreamAllTransactionsServer,
) error {
	n.metrics.notifierActiveStreams.Inc()
	defer n.metrics.notifierActiveStreams.Dec()

	// Create stream context with cancellation
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	// Create stream state with filters from request
	s := &allTxStream{
		filterNamespaces:     req.FilterNamespaces,
		filterStatuses:       req.FilterStatus,
		includeReadWriteSets: req.IncludeReadWriteSets,
		includeEndorsements:  req.IncludeEndorsements,
		includeMetadata:      req.IncludeMetadata,
		blockQueue:           make(chan *committedBlockWithTxs, n.bufferSize),
		ctx:                  ctx,
		cancel:               cancel,
	}

	// Register stream to receive blocks
	n.registerAllTxStream(s)
	defer n.unregisterAllTxStream(s)

	// Run worker to process blocks and send to client
	// The worker handles filtering, enrichment, and sending
	return wrapNotifierError(s.streamWorker(stream))
}

// wrapNotifierError wraps notifier errors with appropriate gRPC status codes.
func wrapNotifierError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return grpcerror.WrapCancelled(err)
	}
	return grpcerror.WrapInternalError(err)
}

func fixTimeout(request *committerpb.NotificationRequest, maxTimeout time.Duration) {
	timeout := min(request.Timeout.AsDuration(), maxTimeout)
	if timeout <= 0 {
		timeout = maxTimeout
	}
	request.Timeout = durationpb.New(timeout)
}

// addRequest adds tx IDs from the request to the subscriptions up to available slots.
// It returns any tx IDs that were rejected due to the limit being reached,
// and the number of new unique txIDs added to the map.
func (m *subscriptions) addRequest(req *notificationRequest) (rejected []string, uniqueNew int) {
	txIDs := req.request.TxStatusRequest.GetTxIds()
	for i, id := range txIDs {
		requests, ok := m.txIDToRequests[id]
		if ok {
			if _, alreadyAssigned := requests[req]; alreadyAssigned {
				continue
			}
		}
		if m.availableSlots <= 0 {
			return txIDs[i:], uniqueNew
		}
		if !ok {
			requests = make(map[*notificationRequest]any)
			m.txIDToRequests[id] = requests
			uniqueNew++
		}
		requests[req] = nil
		req.pending++
		m.availableSlots--
	}
	return nil, uniqueNew
}

// removeAndEnqueueStatusEvents removes TXs from the subscriptions and reports the responses to the subscribers.
// Returns the number of pending txIDs removed across all requests, and the number of unique txIDs removed from the map.
func (m *subscriptions) removeAndEnqueueStatusEvents(
	status []*committerpb.TxStatus,
) (pendingTxIDsRemoved, uniquePendingTxIDsRemoved int) {
	respMap := make(map[channel.Writer[*committerpb.NotificationResponse]][]*committerpb.TxStatus)
	for _, response := range status {
		reqList, ok := m.txIDToRequests[response.Ref.TxId]
		if !ok {
			continue
		}
		delete(m.txIDToRequests, response.Ref.TxId)
		uniquePendingTxIDsRemoved++
		for req := range reqList {
			respMap[req.streamEventQueue] = append(respMap[req.streamEventQueue], response)
			req.pending--
			m.availableSlots++
			pendingTxIDsRemoved++
			if req.pending == 0 {
				req.timer.Stop()
			}
		}
	}
	for notificationQueue, response := range respMap {
		// TODO: Timeout - client is too slow, cancel the stream.
		notificationQueue.WriteWithTimeout(&committerpb.NotificationResponse{
			TxStatusEvents: response,
		}, m.streamWriteTimeout)
	}
	return pendingTxIDsRemoved, uniquePendingTxIDsRemoved
}

// removeAndEnqueueTimeoutEvents removes a request from the subscriptions, and
// reports the timed out TX IDs for this request.
// Returns the number of pending txIDs removed for this request, and the number of unique txIDs removed from the map.
func (m *subscriptions) removeAndEnqueueTimeoutEvents(
	req *notificationRequest,
) (pendingTxIDsRemoved, uniquePendingTxIDsRemoved int) {
	txIDs := make([]string, 0, len(req.request.TxStatusRequest.TxIds))
	for _, id := range req.request.TxStatusRequest.TxIds {
		requests, ok := m.txIDToRequests[id]
		if !ok {
			continue
		}
		// Check that this request is actually subscribed to this txID before appending.
		// A txID may exist in the map from other subscribers but not from this request
		// (e.g., if it was rejected by the global active limit).
		if _, ok = requests[req]; !ok {
			continue
		}
		txIDs = append(txIDs, id)
		pendingTxIDsRemoved++
		m.availableSlots++
		if len(requests) == 1 {
			delete(m.txIDToRequests, id)
			uniquePendingTxIDsRemoved++
		} else {
			delete(requests, req)
		}
	}
	// txIDs can be empty when the timeout fires after a status event has already
	// removed all of this request's subscriptions (i.e., removeAndEnqueueStatusEvents
	// ran first). In that case there is nothing to report, so we skip the write.
	if len(txIDs) > 0 {
		// TODO: Timeout - client is too slow, cancel the stream.
		req.streamEventQueue.WriteWithTimeout(&committerpb.NotificationResponse{
			TimeoutTxIds: txIDs,
		}, m.streamWriteTimeout)
	}
	return pendingTxIDsRemoved, uniquePendingTxIDsRemoved
}

// dispatchBlockToAllTxStreams dispatches a committed block to all registered
// StreamAllTransactions clients. It handles slow clients by using a timeout
// and canceling streams that cannot keep up.
func (n *notifier) dispatchBlockToAllTxStreams(ctx context.Context, block *committedBlockWithTxs) {
	// Get snapshot of streams with minimal lock time
	n.allTxStreamsMu.RLock()
	streams := n.allTxStreams
	n.allTxStreamsMu.RUnlock()

	// Write to each stream's channel with configured timeout
	for _, stream := range streams {
		select {
		case <-ctx.Done():
			return
		case <-stream.ctx.Done():
			// Stream already canceled, skip
			continue
		case <-time.After(n.streamWriteTimeout):
			// Timeout - client is too slow, cancel the stream
			logger.Warnf("Stream write timeout for block %d, closing slow client stream", block.blockNumber)
			stream.cancel()
		case stream.blockQueue <- block:
		}
	}
}

// registerAllTxStream adds a new StreamAllTransactions client to the notifier.
// The stream will receive all committed blocks until it is unregistered or cancelled.
func (n *notifier) registerAllTxStream(stream *allTxStream) {
	n.allTxStreamsMu.Lock()
	defer n.allTxStreamsMu.Unlock()

	// We create a new slice to prevent modifying the slice while
	// it is being iterated over in dispatchBlockToAllTxStreams.
	streams := make([]*allTxStream, len(n.allTxStreams)+1)
	copy(streams, n.allTxStreams)
	streams[len(n.allTxStreams)] = stream
	n.allTxStreams = streams
}

// unregisterAllTxStream removes a StreamAllTransactions client from the notifier.
func (n *notifier) unregisterAllTxStream(stream *allTxStream) {
	n.allTxStreamsMu.Lock()
	defer n.allTxStreamsMu.Unlock()

	idx := slices.Index(n.allTxStreams, stream)
	if idx < 0 {
		return
	}

	// We create a new slice to prevent modifying the slice while
	// it is being iterated over in dispatchBlockToAllTxStreams.
	streams := make([]*allTxStream, len(n.allTxStreams)-1)
	copy(streams, n.allTxStreams[:idx])
	copy(streams[idx:], n.allTxStreams[idx+1:])
	n.allTxStreams = streams
}

// streamWorker runs in its own goroutine for each StreamAllTransactions client.
// It receives committed blocks from the blockQueue, filters and enriches them
// according to the stream's configuration, and sends the results to the client.
func (s *allTxStream) streamWorker(stream committerpb.Notifier_StreamAllTransactionsServer) error {
	q := channel.NewReader(s.ctx, s.blockQueue)
	for {
		block, ok := q.Read()
		if !ok {
			return errors.New("block queue closed")
		}

		// Filter and build transactions in this worker thread
		filteredEvents := s.filterAndBuildEvents(block)
		// Only send if there are events after filtering
		if len(filteredEvents) == 0 {
			continue
		}

		batch := &committerpb.TxEventBatch{
			BlockNumber: block.blockNumber,
			Events:      filteredEvents,
		}
		if err := stream.Send(batch); err != nil {
			return errors.Wrap(err, "failed to send transaction batch")
		}
	}
}

// filterAndBuildEvents processes all transactions in a block, applying filters
// and enriching with optional content based on the stream's configuration.
func (s *allTxStream) filterAndBuildEvents(
	block *committedBlockWithTxs,
) []*committerpb.TxEvent {
	// Pre-allocate with capacity for efficiency
	events := make([]*committerpb.TxEvent, 0, len(block.txs))

	for i, tx := range block.txs {
		status := block.statuses[i]
		// Apply status filter
		if len(s.filterStatuses) > 0 && !slices.Contains(s.filterStatuses, status) {
			continue // Filtered out by status
		}
		matchingNamespaces := filterNamespaces(tx, s.filterNamespaces)
		// If no namespaces matched, filter out this transaction
		if len(s.filterNamespaces) > 0 && len(matchingNamespaces) == 0 {
			continue // Filtered out by namespace
		}

		// Build the event
		event := &committerpb.TxEvent{
			Ref:    tx.Ref,
			Status: status,
		}
		// Include namespaces (read/write sets) if requested
		if s.includeReadWriteSets {
			event.Namespaces = matchingNamespaces
		}
		// Include endorsements if requested
		if s.includeEndorsements {
			event.Endorsements = tx.Content.Endorsements
		}
		if s.includeMetadata {
			event.Metadata = tx.Content.Metadata
		}
		events = append(events, event)
	}

	return events
}

func filterNamespaces(
	tx *servicepb.TxWithRef, filterNamespaces []string,
) []*applicationpb.TxNamespace {
	if tx == nil || tx.Content == nil {
		return nil
	}
	allNamespaces := tx.Content.Namespaces
	if len(filterNamespaces) == 0 {
		// No namespace filter - include all namespaces
		return allNamespaces
	}
	// Apply namespace filter (OR logic: transaction must touch at least one filtered namespace)
	// Filter namespaces - only include those that match the filter
	matchingNamespaces := make([]*applicationpb.TxNamespace, 0, len(filterNamespaces))
	for _, filterNs := range filterNamespaces {
		idx := slices.IndexFunc(allNamespaces, func(ns *applicationpb.TxNamespace) bool {
			return ns.NsId == filterNs
		})
		if idx < 0 {
			continue
		}
		matchingNamespaces = append(matchingNamespaces, allNamespaces[idx])
	}
	return matchingNamespaces
}
