/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

//nolint:paralleltest // Cannot parallelize due to logger.
func TestHealthcheckCMD(t *testing.T) {
	newSystemConfig := func(endpoint *connection.Endpoint) config.SystemConfig {
		dummy := connection.Endpoint{Host: "localhost", Port: 1}
		dummyServiceConfig := []config.ServiceConfig{{GrpcEndpoint: &dummy}}
		return config.SystemConfig{
			ThisService: config.ServiceConfig{
				GrpcEndpoint: endpoint,
			},
			Services: config.SystemServices{
				Verifier:    dummyServiceConfig,
				VCService:   dummyServiceConfig,
				Orderer:     dummyServiceConfig,
				Coordinator: dummyServiceConfig[0],
			},
			DB: config.DatabaseConfig{
				Endpoints: []*connection.Endpoint{&dummy},
			},
			Policy:     &workload.PolicyProfile{ArtifactsPath: t.TempDir()},
			LedgerPath: t.TempDir(),
		}
	}

	// SERVING: start a gRPC server that reports SERVING.
	serverConfig := test.NewLocalHostServer(test.InsecureTLSConfig)
	test.RunGrpcServerForTest(t.Context(), t, serverConfig, nil)
	servingSystem := newSystemConfig(&serverConfig.Endpoint)

	// NOT SERVING: start a separate gRPC server that reports NOT_SERVING.
	notServingConfig := test.NewLocalHostServer(test.InsecureTLSConfig)
	test.RunGrpcServerForTest(t.Context(), t, notServingConfig, func(server *grpc.Server) {
		hs := health.NewServer()
		hs.SetServingStatus("", healthgrpc.HealthCheckResponse_NOT_SERVING)
		healthgrpc.RegisterHealthServer(server, hs)
	})
	notServingSystem := newSystemConfig(&notServingConfig.Endpoint)

	for _, sc := range []struct {
		service string
		name    string
		templ   string
	}{
		{service: sidecarService, name: serviceNames[sidecarService], templ: config.TemplateSidecar},
		{service: coordinatorService, name: serviceNames[coordinatorService], templ: config.TemplateCoordinator},
		{service: vcService, name: serviceNames[vcService], templ: config.TemplateVC},
		{service: verifierService, name: serviceNames[verifierService], templ: config.TemplateVerifier},
		{service: queryService, name: serviceNames[queryService], templ: config.TemplateQueryService},
	} {
		t.Run(fmt.Sprintf("%s/serving", sc.name), func(t *testing.T) {
			cliutil.UnitTestRunner(t, committerCMD(), cliutil.CommandTest{
				Name:              "healthcheck",
				Args:              []string{"healthcheck", sc.service},
				CmdStdOutput:      fmt.Sprintf("%s: SERVING", sc.name),
				UseConfigTemplate: sc.templ,
				System:            servingSystem,
			})
		})

		t.Run(fmt.Sprintf("%s/not-serving", sc.name), func(t *testing.T) {
			cliutil.UnitTestRunner(t, committerCMD(), cliutil.CommandTest{
				Name:              "healthcheck",
				Args:              []string{"healthcheck", sc.service},
				CmdStdErrOutput:   fmt.Sprintf("%s: NOT SERVING", sc.name),
				Err:               errors.New("service is NOT_SERVING"),
				UseConfigTemplate: sc.templ,
				System:            notServingSystem,
			})
		})
	}
}
