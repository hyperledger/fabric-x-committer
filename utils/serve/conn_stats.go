/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"context"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/stats"
)

// ConnStatsHandler is a gRPC stats.Handler that reports the number of open
// connections on the server.
//
// It holds a prometheus.Gauge incremented when a client connects and decremented when it disconnects,
// so the gauge always reflects the number of connections currently open on the server.
//
// The gauge is held in an atomic pointer, so RegisterConnStatHandler is safe to
// call while or after the server starts serving connections.
type ConnStatsHandler struct {
	activeConnections atomic.Pointer[prometheus.Gauge]
}

func newConnStatsHandler() *ConnStatsHandler {
	return &ConnStatsHandler{}
}

// RegisterConnStatHandler wires the gauge that the handler updates on every connection begin and end.
// Until a gauge is registered, the handler is a no-op.
func RegisterConnStatHandler(h *ConnStatsHandler, activeConnections prometheus.Gauge) {
	h.activeConnections.Store(&activeConnections)
}

// HandleConn tracks the connection lifecycle: the gauge is incremented when the
// server accepts a connection and decremented when the server tears it down
// (a client disconnects, keep-alive timeout, max-age, or shutdown).
func (h *ConnStatsHandler) HandleConn(_ context.Context, s stats.ConnStats) {
	g := h.activeConnections.Load()

	if g == nil || *g == nil {
		return
	}

	activeConnections := *g

	switch s.(type) {
	case *stats.ConnBegin:
		activeConnections.Inc()
	case *stats.ConnEnd:
		activeConnections.Dec()
	default:
	}
}

// TagConn is required by stats.Handler; it is a no-op.
func (*ConnStatsHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

// TagRPC is required by stats.Handler; it is a no-op.
func (*ConnStatsHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

// HandleRPC is required by stats.Handler; RPC-level stats are not tracked.
func (*ConnStatsHandler) HandleRPC(context.Context, stats.RPCStats) {}
