/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/query"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
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
	switch name {
	case sidecarService:
		conf, err := config.ReadSidecarYamlAndSetupLogging(config.NewViperWithSidecarDefaults(), configPath)
		if err != nil {
			return err
		}
		service, err := sidecar.New(conf)
		if err != nil {
			return errors.Wrap(err, "failed to create sidecar service")
		}
		defer service.Close()
		return connection.StartService(ctx, service, conf.Server)

	case coordinatorService:
		conf, err := config.ReadCoordinatorYamlAndSetupLogging(config.NewViperWithCoordinatorDefaults(), configPath)
		if err != nil {
			return err
		}
		return connection.StartService(ctx, coordinator.NewCoordinatorService(conf), conf.Server)

	case vcService:
		conf, err := config.ReadVCYamlAndSetupLogging(config.NewViperWithVCDefaults(), configPath)
		if err != nil {
			return err
		}
		service, err := vc.NewValidatorCommitterService(ctx, conf)
		if err != nil {
			return errors.Wrap(err, "failed to create validator committer service")
		}
		defer service.Close()
		return connection.StartService(ctx, service, conf.Server)

	case verifierService:
		conf, err := config.ReadVerifierYamlAndSetupLogging(config.NewViperWithVerifierDefaults(), configPath)
		if err != nil {
			return err
		}
		return connection.StartService(ctx, verifier.New(conf), conf.Server)

	case queryService:
		conf, err := config.ReadQueryYamlAndSetupLogging(config.NewViperWithQueryDefaults(), configPath)
		if err != nil {
			return err
		}
		return connection.StartService(ctx, query.NewQueryService(conf), conf.Server)

	default:
		panic("unknown service: " + name)
	}
}
