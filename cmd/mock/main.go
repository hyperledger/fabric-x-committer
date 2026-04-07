/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"fmt"
	"os"

	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/grpcservice"
)

const (
	mockCmdName         = "mock"
	mockOrdererName     = "mock-ordering-service"
	mockCoordinatorName = "mock-coordinator-service"
	mockVerifierName    = "mock-verifier-service"
	mockVcName          = "mock-vc-service"
)

func main() {
	cmd := mockCMD()

	// On failure, Cobra prints the usage message and error string, so we only
	// need to exit with a non-0 status
	if cmd.Execute() != nil {
		os.Exit(1)
	}
}

func mockCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   mockCmdName,
		Short: "Fabric-X services mock.",
	}
	cmd.AddCommand(cliutil.VersionCmd())
	cmd.AddCommand(mockStartCMD())
	return cmd
}

func mockStartCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a mock service.",
	}
	cmd.AddCommand(startMockOrderer())
	cmd.AddCommand(startMockCoordinator())
	cmd.AddCommand(startMockVerifier())
	cmd.AddCommand(startMockVC())
	return cmd
}

func startMockOrderer() *cobra.Command {
	v := config.NewViperWithLoggingDefault()
	var configPath string
	cmd := &cobra.Command{
		Use:   "orderer",
		Short: fmt.Sprintf("Starts %v.", mockOrdererName),
		Long:  fmt.Sprintf("%v is a mock ordering service.", mockOrdererName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := config.ReadMockOrdererYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", mockOrdererName)
			defer cmd.Printf("%v ended\n", mockOrdererName)

			service, err := mock.NewMockOrderer(conf)
			if err != nil {
				return errors.Wrap(err, "failed to create mock ordering service")
			}
			serverConfigs := conf.ServerConfigs
			if conf.Server != nil && !conf.Server.Endpoint.Empty() {
				serverConfigs = append(serverConfigs, conf.Server)
			}
			return grpcservice.StartAndServe(cmd.Context(), service, serverConfigs...)
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	return cmd
}

func startMockCoordinator() *cobra.Command {
	v := config.NewViperWithCoordinatorDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "coordinator",
		Short: fmt.Sprintf("Starts %v", mockCoordinatorName),
		Long:  fmt.Sprintf("%v is a mock coordinator service.", mockCoordinatorName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := config.ReadCoordinatorYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", mockVerifierName)
			defer cmd.Printf("%v ended\n", mockVerifierName)

			service := mock.NewMockCoordinator()
			return grpcservice.MockServe(cmd.Context(), service, conf.Server)
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	return cmd
}

func startMockVerifier() *cobra.Command {
	v := config.NewViperWithVerifierDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "verifier",
		Short: fmt.Sprintf("Starts %v", mockVerifierName),
		Long:  fmt.Sprintf("%v is a mock signature verification service.", mockVerifierName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := config.ReadVerifierYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", mockVerifierName)
			defer cmd.Printf("%v ended\n", mockVerifierName)

			sv := mock.NewMockSigVerifier()
			return grpcservice.MockServe(cmd.Context(), sv, conf.Server)
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	return cmd
}

func startMockVC() *cobra.Command {
	v := config.NewViperWithVCDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "vc",
		Short: fmt.Sprintf("Starts %v.", mockVcName),
		Long:  fmt.Sprintf("%v is a mock validator and committer service.", mockVcName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := config.ReadVCYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", mockVcName)
			defer cmd.Printf("%v ended\n", mockVcName)

			vcs := mock.NewMockVcService()
			return grpcservice.MockServe(cmd.Context(), vcs, conf.Server)
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	return cmd
}
