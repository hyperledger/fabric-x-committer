/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/durationpb"

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
		metrics            *perfMetrics
		// requestQueue receives requests from users.
		requestQueue chan *notificationRequest
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
		txIDToRequests map[string]map[*notificationRequest]any
		availableSlots int
	}
)

func newNotifier(bufferSize int, conf *NotificationServiceConfig, metrics *perfMetrics) *notifier {
	return &notifier{
		bufferSize:         bufferSize,
		maxTimeout:         conf.MaxTimeout,
		maxActiveTxIDs:     conf.MaxActiveTxIDs,
		maxTxIDsPerRequest: conf.MaxTxIDsPerRequest,
		metrics:            metrics,
		requestQueue:       make(chan *notificationRequest, bufferSize),
	}
}

func (n *notifier) run(ctx context.Context, statusQueue chan []*committerpb.TxStatus) error {
	notifyMap := subscriptions{
		txIDToRequests: make(map[string]map[*notificationRequest]any),
		availableSlots: n.maxActiveTxIDs,
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
					"request contains %d tx IDs, max allowed is %d", len(txIDs), n.maxTxIDsPerRequest))
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
			removed, uniqueRemoved := notifyMap.removeAndEnqueueStatusEvents(status)
			n.recordRemovals(removed, uniqueRemoved)
			promutil.AddToCounter(n.metrics.notifierTxIDsStatusDeliveries, removed)
		case req := <-timeoutQueue:
			removed, uniqueRemoved := notifyMap.removeAndEnqueueTimeoutEvents(req)
			n.recordRemovals(removed, uniqueRemoved)
			promutil.AddToCounter(n.metrics.notifierTxIDsTimeoutDeliveries, removed)
		}
	}
}

func (n *notifier) recordRemovals(removed, uniqueRemoved int) {
	promutil.AddToGauge(n.metrics.notifierPendingTxIDs, -removed)
	promutil.AddToGauge(n.metrics.notifierUniquePendingTxIDs, -uniqueRemoved)
}

// OpenNotificationStream implements the [protonotify.NotifierServer] API.
func (n *notifier) OpenNotificationStream(stream committerpb.Notifier_OpenNotificationStreamServer) error {
	n.metrics.notifierActiveStreams.Inc()
	defer n.metrics.notifierActiveStreams.Dec()

	g, gCtx := errgroup.WithContext(stream.Context())
	globalRequestQueue := channel.NewWriter(gCtx, n.requestQueue)
	streamEventQueue := channel.Make[*committerpb.NotificationResponse](gCtx, n.bufferSize)

	g.Go(func() error {
		for gCtx.Err() == nil {
			req, err := stream.Recv()
			if err != nil {
				return errors.Wrap(err, "error receiving request")
			}
			fixTimeout(req, n.maxTimeout)
			globalRequestQueue.Write(&notificationRequest{
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
// Returns the number of (txID, request) subscriptions enqueued and the number of unique txIDs removed from the map.
func (m *subscriptions) removeAndEnqueueStatusEvents(status []*committerpb.TxStatus) (removed, uniqueRemoved int) {
	respMap := make(map[channel.Writer[*committerpb.NotificationResponse]][]*committerpb.TxStatus)
	for _, response := range status {
		reqList, ok := m.txIDToRequests[response.Ref.TxId]
		if !ok {
			continue
		}
		delete(m.txIDToRequests, response.Ref.TxId)
		uniqueRemoved++
		for req := range reqList {
			respMap[req.streamEventQueue] = append(respMap[req.streamEventQueue], response)
			req.pending--
			m.availableSlots++
			removed++
			if req.pending == 0 {
				req.timer.Stop()
			}
		}
	}
	for notificationQueue, response := range respMap {
		notificationQueue.Write(&committerpb.NotificationResponse{
			TxStatusEvents: response,
		})
	}
	return removed, uniqueRemoved
}

// removeAndEnqueueTimeoutEvents removes a request from the subscriptions, and
// reports the timed out TX IDs for this request.
// Returns the number of enqueued (txID, request) subscriptions and the number of unique txIDs removed from the map.
func (m *subscriptions) removeAndEnqueueTimeoutEvents(req *notificationRequest) (removed, uniqueRemoved int) {
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
		removed++
		m.availableSlots++
		if len(requests) == 1 {
			delete(m.txIDToRequests, id)
			uniqueRemoved++
		} else {
			delete(requests, req)
		}
	}
	req.streamEventQueue.Write(&committerpb.NotificationResponse{
		TimeoutTxIds: txIDs,
	})
	return removed, uniqueRemoved
}
