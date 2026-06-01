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
	cliutil.SetDurationFlag(cmd, "timeout", &timeout)
	return cmd
}

// runDatabaseInitialization reads the database-initialization config,
// connects directly to the database, and initializes it.
func runDatabaseInitialization(cmd *cobra.Command, configPath string, timeout time.Duration) error {
	dbConfig, _, err := config.ReadDBInitYamlAndSetupLogging(config.NewViperWithDBInitDefaults(), configPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	pool, err := db.NewPool(ctx, dbConfig)
	if err != nil {
		return errors.Wrap(err, "failed to connect to database")
	}
	defer pool.Close()

	tablePreSplitTablets, err := db.GetTablePreSplitTablets(ctx, pool, dbConfig)
	if err != nil {
		return errors.Wrap(err, "failed to determine database type")
	}

	if err := db.SetupSystemTablesAndNamespaces(ctx, pool, dbConfig.Retry, tablePreSplitTablets); err != nil {
		return errors.Wrap(err, "failed to initialize state database")
	}

	cmd.Print("Database initialized successfully")
	return nil
}
