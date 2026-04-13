/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/query"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcservice"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

func startCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a service.",
	}
	for _, name := range []string{sidecarService, coordinatorService, vcService, verifierService, queryService} {
		cmd.AddCommand(startServiceCommand(name))
	}
	return cmd
}

func startServiceCommand(name string) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:          name,
		Short:        fmt.Sprintf("Starts %v.", serviceNames[name]),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Printf("Starting %v\n", serviceNames[name])
			defer cmd.Printf("%v ended\n", serviceNames[name])
			return startService(cmd.Context(), name, configPath)
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	return cmd
}

func startService(ctx context.Context, name, configPath string) error {
	conf, err := readConfig(name, configPath)
	if err != nil {
		return err
	}

	metricsProvider := monitoring.NewMetricsProvider()
	if err := startMonitoringServer(ctx, conf, metricsProvider.Registry()); err != nil {
		return err
	}

	switch c := conf.(type) {
	case *sidecar.Config:
		service, err := sidecar.New(c, metricsProvider)
		if err != nil {
			return errors.Wrap(err, "failed to create sidecar service")
		}
		defer service.Close()
		return grpcservice.StartAndServe(ctx, service, c.Server)

	case *coordinator.Config:
		return grpcservice.StartAndServe(ctx, coordinator.NewCoordinatorService(c, metricsProvider), c.Server)

	case *vc.Config:
		service, err := vc.NewValidatorCommitterService(ctx, c, metricsProvider)
		if err != nil {
			return errors.Wrap(err, "failed to create validator committer service")
		}
		defer service.Close()
		return grpcservice.StartAndServe(ctx, service, c.Server)

	case *verifier.Config:
		return grpcservice.StartAndServe(ctx, verifier.New(c, metricsProvider), c.Server)

	case *query.Config:
		return grpcservice.StartAndServe(ctx, query.NewQueryService(c, metricsProvider), c.Server)

	default:
		return errors.Newf("unknown config type: %T", conf)
	}
}

func startMonitoringServer(ctx context.Context, conf any, registry *prometheus.Registry) error {
	var monitoringConf *connection.ServerConfig
	switch c := conf.(type) {
	case *sidecar.Config:
		monitoringConf = c.Monitoring
	case *coordinator.Config:
		monitoringConf = c.Monitoring
	case *vc.Config:
		monitoringConf = c.Monitoring
	case *verifier.Config:
		monitoringConf = c.Monitoring
	case *query.Config:
		monitoringConf = c.Monitoring
	}

	return monitoring.StartHTTPServer(ctx, monitoringConf, registry)
}
