/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/db"
)

const initDBCommand = "init-db"

// databaseInitializationCMD creates the "init-db" command.
func databaseInitializationCMD() *cobra.Command {
	var configPath string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   initDBCommand,
		Short: "Initialize the database with required tables and namespaces",
		Long: `Initialize the state database by creating system tables, metadata tables,
				and system namespaces (meta and config). This is a one-time administrative
				operation that must be performed before doing any operation against the database`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDatabaseInitialization(cmd, configPath, timeout)
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	// registers a duration flag for database initialization.
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Timeout for the initialization operation")
	return cmd
}

// runDatabaseInitialization reads the validator-committer config,
// connects directly to the database using its database section, and initializes it.
func runDatabaseInitialization(cmd *cobra.Command, configPath string, timeout time.Duration) error {
	vcConfig, _, err := config.ReadVCYamlAndSetupLogging(config.NewViperWithVCDefaults(), configPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	if err := db.SetupSystemTablesAndNamespaces(ctx, vcConfig.Database); err != nil {
		return errors.Wrap(err, "failed to initialize state database")
	}

	cmd.Print("Database initialized successfully")
	return nil
}
