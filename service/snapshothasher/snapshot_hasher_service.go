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

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

var logger = flogging.MustGetLogger("snapshot-hasher")

// Service is the snapshot hash scheduler. It exposes no RPCs of its own: work
// arrives through the state database, and the gRPC server exists only for health
// checking, matching how every other service is probed.
type Service struct {
	config      *Config
	metrics     *perfMetrics
	healthcheck *health.Server
}

// NewSnapshotHasherService creates a new snapshot hasher service given a configuration.
func NewSnapshotHasherService(config *Config) *Service {
	return &Service{
		config:      config,
		metrics:     newSnapshotHasherMetrics(),
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

	scheduler := &scheduler{
		state:        statedb.NewSnapshotStateManager(pool, s.config.Database.Retry),
		hasher:       newHasher(s.config),
		metrics:      s.metrics,
		pollInterval: s.config.PollInterval,
	}

	return scheduler.run(ctx)
}

// WaitForReady is always true: the readiness handshake exists to keep the gRPC server
// from serving before a service can answer requests, and this service answers none --
// work reaches it through the state database, and its only RPC is the health check,
// which needs nothing from Run.
func (*Service) WaitForReady(context.Context) bool {
	return true
}

// RegisterService registers the health and monitoring endpoints.
func (s *Service) RegisterService(srv serve.Servers) {
	healthgrpc.RegisterHealthServer(srv.GRPC, s.healthcheck)
	monitoring.RegisterMonitoringServer(srv.HTTP, s.metrics.Provider)
	serve.RegisterServerMetrics(srv.StatsHandler, s.metrics.serverMetrics)
}
