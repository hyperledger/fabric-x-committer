/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// ChannelLenGaugeVec reports the length of each registered channel as its own series, labelled by
// the values it was registered under. Use it for a queue that exists per stream or per worker,
// where the set of live queues changes at runtime: a series exists only while its channel is
// registered, so a finished stream leaves no stale reading behind, and an operator sums over the
// label to get the total across workers.
type ChannelLenGaugeVec[T any] struct {
	desc      *prometheus.Desc
	labels    []string
	mu        sync.RWMutex
	channels  map[string]channelWithLabels[T]
	separator string
}

type channelWithLabels[T any] struct {
	ch          <-chan T
	labelValues []string
}

// NewChannelLenGaugeVec creates a gauge collector for a queue that exists once per stream or
// worker. Registering and unregistering a channel adds and removes its series.
func NewChannelLenGaugeVec[T any](
	p *Provider,
	opts prometheus.GaugeOpts,
	labels []string,
) *ChannelLenGaugeVec[T] {
	c := &ChannelLenGaugeVec[T]{
		desc: prometheus.NewDesc(
			prometheus.BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
			opts.Help,
			labels,
			opts.ConstLabels,
		),
		labels:   labels,
		channels: make(map[string]channelWithLabels[T]),
		// A separator that cannot appear in a label value keeps the map key unambiguous.
		separator: "\x00",
	}
	p.registry.MustRegister(c)
	return c
}

// Register starts reporting ch under the given label values. Registering the same label values
// again replaces the channel, so the values must identify the queue uniquely: two live queues
// sharing them would otherwise export one series and hide the other.
func (c *ChannelLenGaugeVec[T]) Register(ch <-chan T, labelValues ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channels[strings.Join(labelValues, c.separator)] = channelWithLabels[T]{
		ch:          ch,
		labelValues: labelValues,
	}
}

// Unregister stops reporting the channel registered under the given label values, removing its
// series. It is safe to call for label values that were never registered.
func (c *ChannelLenGaugeVec[T]) Unregister(labelValues ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.channels, strings.Join(labelValues, c.separator))
}

// Describe implements prometheus.Collector.
func (c *ChannelLenGaugeVec[T]) Describe(out chan<- *prometheus.Desc) {
	out <- c.desc
}

// Collect implements prometheus.Collector. It reports one gauge per registered channel.
func (c *ChannelLenGaugeVec[T]) Collect(out chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, entry := range c.channels {
		out <- prometheus.MustNewConstMetric(
			c.desc, prometheus.GaugeValue, float64(len(entry.ch)), entry.labelValues...,
		)
	}
}
