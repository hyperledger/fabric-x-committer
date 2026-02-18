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

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
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
	cmd.AddCommand(config.VersionCmd())
	cmd.AddCommand(mockOrdererCMD())
	cmd.AddCommand(mockCoordinatorCMD())
	cmd.AddCommand(mockVerifierCMD())
	cmd.AddCommand(mockVcCMD())
	return cmd
}

func mockOrdererCMD() *cobra.Command {
	v := config.NewViperWithLoggingDefault()
	var configPath string
	cmd := &cobra.Command{
		Use:   "start-orderer",
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
			return connection.StartService(cmd.Context(), service, serverConfigs...)
		},
	}
	utils.Must(config.SetDefaultFlags(v, cmd, &configPath))
	return cmd
}

func mockCoordinatorCMD() *cobra.Command {
	v := config.NewViperWithCoordinatorDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "start-coordinator",
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
			return connection.RunGrpcServer(cmd.Context(), conf.Server, service.RegisterService)
		},
	}
	utils.Must(config.SetDefaultFlags(v, cmd, &configPath))
	return cmd
}

//nolint:dupl // similar to mockVcCMD.
func mockVerifierCMD() *cobra.Command {
	v := config.NewViperWithVerifierDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "start-verifier",
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
			return connection.RunGrpcServer(cmd.Context(), conf.Server, sv.RegisterService)
		},
	}
	utils.Must(config.SetDefaultFlags(v, cmd, &configPath))
	return cmd
}

//nolint:dupl // similar to mockVerifierCMD.
func mockVcCMD() *cobra.Command {
	v := config.NewViperWithVCDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "start-vc",
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
			return connection.RunGrpcServer(cmd.Context(), conf.Server, vcs.RegisterService)
		},
	}
	utils.Must(config.SetDefaultFlags(v, cmd, &configPath))
	return cmd
}
