/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/loadgen"
	"github.com/hyperledger/fabric-x-committer/loadgen/adapters"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const (
	serviceName = "loadgen"
)

func main() {
	cmd := loadgenCmd()

	// On failure, Cobra prints the usage message and error string, so we only
	// need to exit with a non-0 status
	if cmd.Execute() != nil {
		os.Exit(1)
	}
}

func loadgenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   serviceName,
		Short: fmt.Sprintf("%v is a load generator for Fabric-X committer.", serviceName),
	}

	cmd.AddCommand(config.VersionCmd())
	cmd.AddCommand(loadGenCMD())
	cmd.AddCommand(loadGenMaterial())
	return cmd
}

func loadGenCMD() *cobra.Command {
	v := config.NewViperWithLoadGenDefaults()
	var configPath string
	var onlyNamespace bool
	var onlyWorkload bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: fmt.Sprintf("Starts %v.", serviceName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := config.ReadLoadGenYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			cmd.Printf("Starting %v\n", serviceName)
			defer cmd.Printf("%v ended\n", serviceName)

			if onlyNamespace {
				conf.Generate = adapters.Phases{Namespaces: true}
			}
			if onlyWorkload {
				conf.Generate = adapters.Phases{Load: true}
			}

			client, err := loadgen.NewLoadGenClient(conf)
			if err != nil {
				return err
			}
			return connection.StartService(cmd.Context(), client, conf.Server)
		},
	}
	utils.Must(config.SetDefaultFlags(v, cmd, &configPath))
	p := cmd.PersistentFlags()
	p.BoolVar(&onlyNamespace, "only-namespace", false, "only run namespace generation")
	p.BoolVar(&onlyWorkload, "only-workload", false, "only run workload generation")
	return cmd
}

func loadGenMaterial() *cobra.Command {
	v := config.NewViperWithLoadGenDefaults()
	var configPath string
	cmd := &cobra.Command{
		Use:   "make-material",
		Short: "Generates the crypto material and genesis block to the output folder.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := config.ReadLoadGenYamlAndSetupLogging(v, configPath)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true

			cmd.Printf("Generating crypto material...\n")
			err = workload.PrepareCryptoMaterial(&conf.LoadProfile.Policy)
			if err != nil {
				return err
			}
			cmd.Printf("Crypto material path: %s\n", conf.LoadProfile.Policy.CryptoMaterialPath)
			return nil
		},
	}
	utils.Must(config.SetDefaultFlags(v, cmd, &configPath))
	return cmd
}
