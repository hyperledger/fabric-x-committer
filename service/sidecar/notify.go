/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
)

type (
	// notifier uses a single go-routine for processing.
	// Each stream has its own notificationQueue (stream-level).
	// All requests flow into the global requestQueue and are accumulated in subscriptionMap (txID -> requests).
	// Status updates from statusQueue are dispatched by grouping responses per stream and
	// writing to each stream's notificationQueue.
	// Deadlock is prevented by buffered channels and single-threaded processing in run().
	notifier struct {
		committerpb.UnimplementedNotifierServer
		bufferSize int
		maxTimeout time.Duration
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

	// subscriptionMap maps from TX ID to a set of notificationRequest objects.
	subscriptionMap map[string]map[*notificationRequest]any
)

func newNotifier(bufferSize int, conf *NotificationServiceConfig) *notifier {
	maxTimeout := conf.MaxTimeout
	if maxTimeout <= 0 {
		maxTimeout = defaultNotificationMaxTimeout
	}
	return &notifier{
		bufferSize:   bufferSize,
		maxTimeout:   maxTimeout,
		requestQueue: make(chan *notificationRequest, bufferSize),
	}
}

func (n *notifier) run(ctx context.Context, statusQueue chan []*committerpb.TxStatus) error {
	notifyMap := subscriptionMap(make(map[string]map[*notificationRequest]any))
	timeoutQueue := make(chan *notificationRequest, cap(n.requestQueue))
	timeoutQueueCtx := channel.NewWriter(ctx, timeoutQueue)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req := <-n.requestQueue:
			notifyMap.addRequest(req)
			if req.pending > 0 {
				req.timer = time.AfterFunc(req.request.Timeout.AsDuration(), func() {
					timeoutQueueCtx.Write(req)
				})
			}
		case status := <-statusQueue:
			notifyMap.removeAndEnqueueStatusEvents(status)
		case req := <-timeoutQueue:
			notifyMap.removeAndEnqueueTimeoutEvents(req)
		}
	}
}

// OpenNotificationStream implements the [protonotify.NotifierServer] API.
func (n *notifier) OpenNotificationStream(stream committerpb.Notifier_OpenNotificationStreamServer) error {
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

// addRequest adds a requests to the map and updates the number of pending TX IDs for this request.
func (m subscriptionMap) addRequest(req *notificationRequest) {
	for _, id := range req.request.TxStatusRequest.TxIds {
		requests, ok := m[id]
		if !ok {
			requests = make(map[*notificationRequest]any)
			m[id] = requests
		}
		if _, alreadyAssigned := requests[req]; !alreadyAssigned {
			requests[req] = nil
			req.pending++
		}
	}
}

// removeAndEnqueueStatusEvents removes TXs from the map and reports the responses to the subscribers.
func (m subscriptionMap) removeAndEnqueueStatusEvents(status []*committerpb.TxStatus) {
	respMap := make(map[channel.Writer[*committerpb.NotificationResponse]][]*committerpb.TxStatus)
	for _, response := range status {
		reqList, ok := m[response.Ref.TxId]
		if !ok {
			continue
		}
		delete(m, response.Ref.TxId)
		for req := range reqList {
			respMap[req.streamEventQueue] = append(respMap[req.streamEventQueue], response)
			req.pending--
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
}

// removeAndEnqueueTimeoutEvents removes a request from the map, and reports the remaining TX IDs for this request.
func (m subscriptionMap) removeAndEnqueueTimeoutEvents(req *notificationRequest) {
	txIDs := make([]string, 0, len(req.request.TxStatusRequest.TxIds))
	for _, id := range req.request.TxStatusRequest.TxIds {
		requests, ok := m[id]
		if !ok {
			continue
		}
		txIDs = append(txIDs, id)
		if _, ok = requests[req]; !ok {
			continue
		}
		if len(requests) == 1 {
			delete(m, id)
		} else {
			delete(requests, req)
		}
	}
	req.streamEventQueue.Write(&committerpb.NotificationResponse{
		TimeoutTxIds: txIDs,
	})
}
