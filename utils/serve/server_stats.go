/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"context"
	"sync/atomic"

	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

type (
	// ServerStatsHandler is a gRPC stats.Handler attached to every server. It records
	// server-side connection and RPC-level metrics from the gRPC stats callbacks.
	//
	// It holds its ServerMetrics in an atomic pointer and is a no-op until a service registers them,
	// so registration is safe while or after the server starts serving.
	ServerStatsHandler struct {
		metrics atomic.Pointer[ServerMetrics]
	}

	// resolvedMethod carries per-RPC state from TagRPC/Begin to End. HandleRPC stores a pointer
	// to it on the context, so the isStream flag set on Begin is visible to the matching End.
	resolvedMethod struct {
		full     string
		isStream bool
	}

	// rpcCtxKey is defined due to the following linter message: "should not use built-in type string as key for value;
	// define your own type to avoid collisions"
	rpcCtxKey string
)

// rpcContextKey is the context key under which TagRPC stores the resolved method.
const rpcContextKey rpcCtxKey = "rpc-method"

// RegisterServerMetrics gives the handler the metrics to record into. Until this is called the
// handler is a no-op, and it is safe to call while or after the server starts serving.
func RegisterServerMetrics(h *ServerStatsHandler, m *ServerMetrics) {
	h.metrics.Store(m)
}

// TagRPC stores a per-RPC resolvedMethod on the returned context, which gRPC threads through
// the RPC's HandleRPC calls. A pointer is stored so Begin can record the RPC kind for End.
func (*ServerStatsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	return context.WithValue(
		ctx, rpcContextKey, &resolvedMethod{
			full: info.FullMethodName,
		},
	)
}

// HandleRPC records RPC-level metrics on stream beginning and RPC completion.
func (h *ServerStatsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	m := h.metrics.Load()
	if m == nil {
		return
	}
	rm, ok := ctx.Value(rpcContextKey).(*resolvedMethod)
	if !ok || rm == nil {
		return
	}
	switch st := s.(type) {
	case *stats.Begin:
		rm.isStream = st.IsServerStream || st.IsClientStream
		if rm.isStream {
			m.ActiveStreams.WithLabelValues(rm.full).Inc()
		}
	case *stats.End:
		// If the error is nil, the result is "OK"; if it is not a gRPC error, the result is "Unknown".
		statusCode := status.Code(st.Error).String()
		duration := st.EndTime.Sub(st.BeginTime).Seconds()

		m.RequestsTotal.WithLabelValues(rm.full).Inc()

		if rm.isStream {
			m.ActiveStreams.WithLabelValues(rm.full).Dec()
			m.StreamDurationSeconds.WithLabelValues(rm.full, statusCode).Observe(duration)
		} else {
			m.LatencySeconds.WithLabelValues(rm.full, statusCode).Observe(duration)
		}
	default:
	}
}

// HandleConn tracks the connection lifecycle: ActiveConnections is incremented when the server
// accepts a connection and decremented when it tears it down (client disconnect, keep-alive
// timeout, max-age, or shutdown).
func (h *ServerStatsHandler) HandleConn(_ context.Context, s stats.ConnStats) {
	m := h.metrics.Load()
	if m == nil {
		return
	}
	switch s.(type) {
	case *stats.ConnBegin:
		m.ActiveConnections.Inc()
	case *stats.ConnEnd:
		m.ActiveConnections.Dec()
	default:
	}
}

// TagConn is required by stats.Handler.
func (*ServerStatsHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
