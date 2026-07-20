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
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
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
				operation that must be performed before booting the committer for the first time`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dbConfig, _, err := config.ReadDBYamlAndSetupLogging(config.NewViperWithDBDefaults(), configPath)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			if err := statedb.SetupSystemTablesAndNamespaces(ctx, dbConfig); err != nil {
				return errors.Wrap(err, "failed to initialize state database")
			}

			cmd.Print("Database initialized successfully")
			return nil
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	// registers a duration flag for database initialization.
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Timeout for the initialization operation")
	return cmd
}
