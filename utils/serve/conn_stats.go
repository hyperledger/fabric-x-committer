/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"context"
	"sync/atomic"

	"google.golang.org/grpc/status"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/stats"
)

type (
	// ServerStatsHandler is a gRPC stats.Handler attached to every server. It records
	// server-side connection and RPC-level metrics from the gRPC stats callbacks, so
	// services do not hand-write this instrumentation inside their RPC methods.
	//
	// Metrics are held in ServerMetrics, stored in an atomic pointer and a no-op until a service
	// registers it, so registration is safe while or after the server starts serving. Each
	// metric field is optional: a nil field is simply not recorded.
	ServerStatsHandler struct {
		metrics atomic.Pointer[ServerMetrics]
	}

	// ServerMetrics holds the server-side connection and RPC metrics a service records. The RPC
	// metrics are labeled by the full gRPC method ("/pkg.Service/Method"). Every field is
	// A nil field is simply not recorded.
	ServerMetrics struct {
		// RequestsTotal counts completed RPCs (unary and streaming).
		RequestsTotal *prometheus.CounterVec
		// LatencySeconds observes RPC duration, labeled by method and by the RPC's
		// gRPC status code. Streaming RPCs are not observed, as their duration is the lifetime
		// of the stream rather than request latency; StreamDurationSeconds records those instead.
		LatencySeconds *prometheus.HistogramVec
		// StreamDurationSeconds observes how long a streaming RPC ran from start to end, labeled
		// by method and by the stream's gRPC status code.
		StreamDurationSeconds *prometheus.HistogramVec
		// ActiveStreams reflects the number of streaming RPCs currently in progress.
		ActiveStreams *prometheus.GaugeVec
		// ActiveConnections is incremented when the server accepts a connection and decremented
		// when it is torn down, so it reflects the number of connections currently open.
		ActiveConnections prometheus.Gauge
	}

	// resolvedMethod carries per-RPC state from TagRPC/Begin to End. HandleRPC stores a pointer
	// to it on the context, so the isStream flag set on Begin is visible to the matching End.
	resolvedMethod struct {
		full     string
		isStream bool
	}

	rpcCtxKey string
)

// rpcContextKey is the context key under which TagRPC stores the resolved method.
const rpcContextKey rpcCtxKey = "rpc-method"

// RegisterServerMetrics wires the metric families the handler records into. A nil family (or
// never calling this) leaves that family unrecorded, so the handler is a complete no-op until a
// service registers. Safe to call while or after the server starts serving.
func RegisterServerMetrics(h *ServerStatsHandler, m *ServerMetrics) {
	h.metrics.Store(m)
}

// TagRPC stores a per-RPC resolvedMethod on the returned context, which gRPC threads through
// the RPC's HandleRPC calls. A pointer is stored so Begin can record the RPC kind for End.
func (h *ServerStatsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	if h.metrics.Load() == nil {
		return ctx
	}
	return context.WithValue(ctx, rpcContextKey, &resolvedMethod{full: info.FullMethodName})
}

// HandleRPC records RPC-level metrics on stream beginning and RPC completion.
//
//nolint:gocognit // The switch is simple enough to be readable.
func (h *ServerStatsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	m := h.metrics.Load()
	if m == nil {
		return
	}
	rm, ok := ctx.Value(rpcContextKey).(*resolvedMethod)
	if !ok {
		return
	}
	switch st := s.(type) {
	case *stats.Begin:
		rm.isStream = st.IsServerStream
		if rm.isStream && m.ActiveStreams != nil {
			m.ActiveStreams.WithLabelValues(rm.full).Inc()
		}
	case *stats.End:
		statusCode := status.Code(st.Error).String()
		duration := st.EndTime.Sub(st.BeginTime).Seconds()
		if m.RequestsTotal != nil {
			m.RequestsTotal.WithLabelValues(rm.full).Inc()
		}
		if !rm.isStream && m.LatencySeconds != nil {
			m.LatencySeconds.WithLabelValues(rm.full, statusCode).Observe(duration)
		}
		if rm.isStream && m.ActiveStreams != nil {
			m.ActiveStreams.WithLabelValues(rm.full).Dec()
		}
		if rm.isStream && m.StreamDurationSeconds != nil {
			m.StreamDurationSeconds.WithLabelValues(rm.full, statusCode).Observe(duration)
		}
	default:
	}
}

// HandleConn tracks the connection lifecycle: ActiveConnections is incremented when the server
// accepts a connection and decremented when it tears it down (client disconnect, keep-alive
// timeout, max-age, or shutdown).
func (h *ServerStatsHandler) HandleConn(_ context.Context, s stats.ConnStats) {
	m := h.metrics.Load()
	if m == nil || m.ActiveConnections == nil {
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
