/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// healthcheckCMD creates the "healthcheck" parent command with all service subcommands.
func healthcheckCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Check if a service is healthy.",
	}
	for _, name := range []string{sidecarService, coordinatorService, vcService, verifierService, queryService} {
		cmd.AddCommand(healthcheckServiceCommand(name))
	}
	return cmd
}

func healthcheckServiceCommand(name string) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:          name,
		Short:        fmt.Sprintf("Check %v health.", serviceNames[name]),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, serverConfig, err := readConfig(name, configPath)
			if err != nil {
				return err
			}

			displayName := serviceNames[name]
			if err := connection.RunHealthCheck(
				cmd.Context(), serverConfig.GRPC.Endpoint, serverConfig.GRPC.TLS,
			); err != nil {
				cmd.PrintErrf("%s: NOT SERVING: %v\n", displayName, err)
				return err
			}
			cmd.Printf("%s: SERVING\n", displayName)
			return nil
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	return cmd
}
