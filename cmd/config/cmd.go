/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"fmt"
	"runtime"

	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/query"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// Service names and version.
const (
	CommitterVersion = "0.0.2"

	CommitterName   = "Committer"
	SidecarName     = "Sidecar"
	CoordinatorName = "Coordinator"
	VcName          = "Validator-Committer"
	VerifierName    = "Verifier"
	QueryName       = "Query-Service"
)

// VersionCmd creates a version command.
func VersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "version",
		Short:        fmt.Sprintf("print %s version", CommitterName),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Printf("%s\n", FullCommitterVersion())
			return nil
		},
	}
}

// FullCommitterVersion returns the committer version string.
func FullCommitterVersion() string {
	return fmt.Sprintf("%s version %s %s/%s", CommitterName, CommitterVersion, runtime.GOOS, runtime.GOARCH)
}

// StartCMD creates the "start" parent command with all service subcommands.
func StartCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a service.",
	}
	cmd.AddCommand(StartSidecar())
	cmd.AddCommand(StartCoordinator())
	cmd.AddCommand(StartVC())
	cmd.AddCommand(StartVerifier())
	cmd.AddCommand(StartQuery())
	return cmd
}

// StartSidecar creates the start sidecar command.
func StartSidecar() *cobra.Command {
	v := NewViperWithSidecarDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "sidecar",
		Short: fmt.Sprintf("Starts %v.", SidecarName),
		Long:  fmt.Sprintf("%v links between the system services.", SidecarName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := ReadSidecarYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", SidecarName)
			defer cmd.Printf("%v ended\n", SidecarName)

			service, err := sidecar.New(conf)
			if err != nil {
				return errors.Wrap(err, "failed to create sidecar service")
			}
			defer service.Close()
			return connection.StartService(cmd.Context(), service, conf.Server)
		},
	}
	utils.Must(SetDefaultFlags(v, cmd, &configPath))
	utils.Must(CobraInt(v, cmd, CobraFlag{
		Name:  "committer-endpoint",
		Usage: "sets the endpoint of the committer in the config file",
		Key:   "committer.endpoint",
	}))
	utils.Must(CobraString(v, cmd, CobraFlag{
		Name:  "ledger-path",
		Usage: "sets the path of the ledger",
		Key:   "ledger.path",
	}))
	return cmd
}

// StartCoordinator creates the start coordinator command.
func StartCoordinator() *cobra.Command {
	v := NewViperWithCoordinatorDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "coordinator",
		Short: fmt.Sprintf("Starts %v.", CoordinatorName),
		Long:  fmt.Sprintf("%v is a transaction flow coordinator.", CoordinatorName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := ReadCoordinatorYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", CoordinatorName)
			defer cmd.Printf("%v ended\n", CoordinatorName)

			service := coordinator.NewCoordinatorService(conf)
			return connection.StartService(cmd.Context(), service, conf.Server)
		},
	}
	utils.Must(SetDefaultFlags(v, cmd, &configPath))
	return cmd
}

// StartVC creates the start validator-committer command.
func StartVC() *cobra.Command {
	v := NewViperWithVCDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "vc",
		Short: fmt.Sprintf("Starts %v.", VcName),
		Long:  fmt.Sprintf("%v is a validator and committer service.", VcName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := ReadVCYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", VcName)
			defer cmd.Printf("%v ended\n", VcName)

			service, err := vc.NewValidatorCommitterService(cmd.Context(), conf)
			if err != nil {
				return errors.Wrap(err, "failed to create validator committer service")
			}
			defer service.Close()
			return connection.StartService(cmd.Context(), service, conf.Server)
		},
	}
	utils.Must(SetDefaultFlags(v, cmd, &configPath))
	return cmd
}

// StartVerifier creates the start verifier command.
func StartVerifier() *cobra.Command {
	v := NewViperWithVerifierDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "verifier",
		Short: fmt.Sprintf("Starts %v.", VerifierName),
		Long:  fmt.Sprintf("%v verifies the transaction's form and signatures.", VerifierName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := ReadVerifierYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", VerifierName)
			defer cmd.Printf("%v ended\n", VerifierName)

			service := verifier.New(conf)
			return connection.StartService(cmd.Context(), service, conf.Server)
		},
	}
	utils.Must(SetDefaultFlags(v, cmd, &configPath))
	utils.Must(CobraInt(v, cmd, CobraFlag{
		Name:  "parallelism",
		Usage: "sets the value of the parallelism in the config file",
		Key:   "parallel-executor.parallelism",
	}))
	utils.Must(CobraInt(v, cmd, CobraFlag{
		Name:  "batch-size-cutoff",
		Usage: "Batch time cutoff limit",
		Key:   "parallel-executor.batch-size-cutoff",
	}))
	utils.Must(CobraInt(v, cmd, CobraFlag{
		Name:  "channel-buffer-size",
		Usage: "Channel buffer size for the executor",
		Key:   "parallel-executor.channel-buffer-size",
	}))
	utils.Must(CobraDuration(v, cmd, CobraFlag{
		Name:  "batch-time-cutoff",
		Usage: "Batch time cutoff limit",
		Key:   "parallel-executor.batch-time-cutoff",
	}))
	return cmd
}

// StartQuery creates the start query command.
func StartQuery() *cobra.Command {
	v := NewViperWithQueryDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "query",
		Short: fmt.Sprintf("Starts %v.", QueryName),
		Long:  fmt.Sprintf("%v is a service to query the state.", QueryName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := ReadQueryYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", QueryName)
			defer cmd.Printf("%v ended\n", QueryName)

			qs := query.NewQueryService(conf)
			return connection.StartService(cmd.Context(), qs, conf.Server)
		},
	}
	utils.Must(SetDefaultFlags(v, cmd, &configPath))
	return cmd
}
