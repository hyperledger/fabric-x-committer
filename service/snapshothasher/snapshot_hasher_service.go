/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Package snapshothasher hosts the snapshot hash scheduler: the single component that
// turns a committed `_snapshot` record into a content hash of its clone database.
//
// It is a service of its own, deployed as ONE instance alongside any number of
// validator-committers, because hashing is neither part of committing a
// transaction nor something several processes should attempt at once. The
// validator-committer's only duty is to make the `_snapshot` record durable
// together with its clone; this service then discovers the work from that durable
// state. Nothing notifies it, so a fresh snapshot, one resubmitted by the
// coordinator, and one orphaned by a restart all reach hashing through the same
// path.
package snapshothasher

import (
	"context"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/snapshotstate"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

var logger = flogging.MustGetLogger("snapshot-hasher")

// Service is the snapshot hash scheduler. It exposes no RPCs of its own: work
// arrives through the state database, and the gRPC server exists only for health
// checking, matching how every other service is probed.
type Service struct {
	config      *Config
	metrics     *perfMetrics
	ready       *channel.Ready
	healthcheck *health.Server
}

// NewSnapshotHasherService creates a new snapshot hasher service given a configuration.
func NewSnapshotHasherService(config *Config) *Service {
	return &Service{
		config:      config,
		metrics:     newSnapshotHasherMetrics(),
		ready:       channel.NewReady(),
		healthcheck: serve.DefaultHealthCheckService(),
	}
}

// Run opens the state-database pool and drives the scheduler until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	pool, err := statedb.NewPool(ctx, s.config.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Infof("snapshot service connected to database at [%s]", s.config.Database.EndpointsString())

	scheduler := newScheduler(&schedulerConfig{
		state:        snapshotstate.NewStateManager(pool, s.config.Database.Retry),
		hasher:       newHasher(s.config),
		metrics:      s.metrics,
		pollInterval: s.config.PollInterval,
	})

	s.ready.SignalReady()
	defer s.ready.Reset()

	return scheduler.run(ctx)
}

// WaitForReady waits for the service resources to initialize, so it is ready to
// hash snapshots. If the context ended before the service is ready, returns false.
func (s *Service) WaitForReady(ctx context.Context) bool {
	return s.ready.WaitForReady(ctx)
}

// RegisterService registers the health and monitoring endpoints.
func (s *Service) RegisterService(srv serve.Servers) {
	healthgrpc.RegisterHealthServer(srv.GRPC, s.healthcheck)
	monitoring.RegisterMonitoringServer(srv.HTTP, s.metrics.Provider)
	serve.RegisterServerMetrics(srv.StatsHandler, s.metrics.serverMetrics)
}
