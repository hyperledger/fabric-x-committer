/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/query"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
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
			return runHealthCheck(cmd, name, configPath)
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	return cmd
}

// runHealthCheck reads the service config, performs a gRPC health check, and prints the result.
func runHealthCheck(cmd *cobra.Command, name, configPath string) error {
	conf, err := readConfig(name, configPath)
	if err != nil {
		return err
	}

	var server *connection.ServerConfig
	switch c := conf.(type) {
	case *sidecar.Config:
		server = c.Server
	case *coordinator.Config:
		server = c.Server
	case *vc.Config:
		server = c.Server
	case *verifier.Config:
		server = c.Server
	case *query.Config:
		server = c.Server
	default:
		return errors.Newf("unknown config type: %T", conf)
	}

	displayName := serviceNames[name]
	if err := connection.RunHealthCheck(cmd.Context(), server.Endpoint, server.TLS); err != nil {
		cmd.PrintErrf("%s: NOT SERVING: %v\n", displayName, err)
		return err
	}
	cmd.Printf("%s: SERVING\n", displayName)
	return nil
}
