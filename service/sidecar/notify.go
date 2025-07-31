/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/api/protonotify"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
)

type (
	notifier struct {
		protonotify.UnimplementedNotifierServer
		bufferSize int
		maxTimeout time.Duration
		// requestQueue receives requests from users.
		requestQueue chan *notificationRequest
		// statusQueue receives statuses from the committer.
		statusQueue chan []*protonotify.TxStatusEvent
	}

	notificationRequest struct {
		request *protonotify.NotificationRequest
		// notificationQueue is used to submit notifications to the users (includes the request's context).
		notificationQueue channel.Writer[*protonotify.NotificationResponse]

		// Internal use. Used to keep track of the request, and release its resources when it is fulfilled.
		timer   *time.Timer
		pending int
	}

	// notificationMap maps from TX ID to a set of notificationRequest objects.
	notificationMap map[string]map[*notificationRequest]any
)

func newNotifier(conf *NotificationServiceConfig) *notifier {
	bufferSize := conf.ChannelBufferSize
	if bufferSize <= 0 {
		bufferSize = defaultNotificationBufferSize
	}
	maxTimeout := conf.MaxTimeout
	if maxTimeout <= 0 {
		maxTimeout = defaultNotificationMaxTimeout
	}
	return &notifier{
		bufferSize:   bufferSize,
		maxTimeout:   maxTimeout,
		requestQueue: make(chan *notificationRequest, bufferSize),
		statusQueue:  make(chan []*protonotify.TxStatusEvent, bufferSize),
	}
}

func (n *notifier) run(ctx context.Context) error {
	notifyMap := notificationMap(make(map[string]map[*notificationRequest]any))
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
		case status := <-n.statusQueue:
			notifyMap.removeWithStatus(status)
		case req := <-timeoutQueue:
			notifyMap.removeWithTimeout(req)
		}
	}
}

// OpenNotificationStream implements the [protonotify.NotifierServer] API.
func (n *notifier) OpenNotificationStream(stream protonotify.Notifier_OpenNotificationStreamServer) error {
	g, gCtx := errgroup.WithContext(stream.Context())
	requestQueue := channel.NewWriter(gCtx, n.requestQueue)
	notificationQueue := channel.Make[*protonotify.NotificationResponse](gCtx, n.bufferSize)

	g.Go(func() error {
		for gCtx.Err() == nil {
			req, err := stream.Recv()
			if err != nil {
				return err
			}
			fixTimeout(req, n.maxTimeout)
			requestQueue.Write(&notificationRequest{
				request:           req,
				notificationQueue: notificationQueue,
			})
		}
		return gCtx.Err()
	})
	g.Go(func() error {
		for gCtx.Err() == nil {
			res, ok := notificationQueue.Read()
			if !ok {
				break
			}
			if err := stream.Send(res); err != nil {
				return err
			}
		}
		return gCtx.Err()
	})
	return g.Wait()
}

func fixTimeout(request *protonotify.NotificationRequest, maxTimeout time.Duration) {
	timeout := min(request.Timeout.AsDuration(), maxTimeout)
	if timeout <= 0 {
		timeout = maxTimeout
	}
	request.Timeout = durationpb.New(timeout)
}

// addRequest adds a requests to the map and updates the number of pending TX IDs for this request.
func (m notificationMap) addRequest(req *notificationRequest) {
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

// removeWithStatus removes TXs from the map and reports the responses to the subscribers.
func (m notificationMap) removeWithStatus(status []*protonotify.TxStatusEvent) {
	respMap := make(map[channel.Writer[*protonotify.NotificationResponse]][]*protonotify.TxStatusEvent)
	for _, response := range status {
		reqList, ok := m[response.TxId]
		if !ok {
			continue
		}
		delete(m, response.TxId)
		for req := range reqList {
			respMap[req.notificationQueue] = append(respMap[req.notificationQueue], response)
			req.pending--
			if req.pending == 0 {
				req.timer.Stop()
			}
		}
	}
	for notificationQueue, response := range respMap {
		notificationQueue.Write(&protonotify.NotificationResponse{
			TxStatusEvents: response,
		})
	}
}

// removeWithTimeout removes a request from the map, and reports the remaining TX IDs for this request.
func (m notificationMap) removeWithTimeout(req *notificationRequest) {
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
	req.notificationQueue.Write(&protonotify.NotificationResponse{
		TimeoutTxIds: txIDs,
	})
}
