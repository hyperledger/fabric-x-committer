/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

const (
	initDBCommand      = "init"
	cloneCommand       = "clone"
	deleteCloneCommand = "delete"
)

// databaseCMD groups state database administration commands.
func databaseCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Administer the state database",
		Args:  cobra.NoArgs,
	}
	clone := &cobra.Command{
		Use:   cloneCommand,
		Short: "List and delete recorded snapshot database clones",
		Args:  cobra.NoArgs,
	}
	clone.AddCommand(listClonesCMD(), deleteCloneCMD())
	cmd.AddCommand(databaseInitializationCMD(), clone)
	return cmd
}

// databaseInitializationCMD creates the "db init" command.
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
			cfg, _, err := config.ReadVCYamlAndSetupLogging(config.NewViperWithVCDefaults(), configPath)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			if err := statedb.SetupSystemTablesAndNamespaces(ctx, cfg.Database); err != nil {
				return errors.Wrap(err, "failed to initialize state database")
			}

			cmd.Println("Database initialized successfully")
			return nil
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	// registers a duration flag for database initialization.
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Timeout for the initialization operation")
	return cmd
}

// listClonesCMD creates the "db clone list" command.
func listClonesCMD() *cobra.Command {
	var configPath string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recorded snapshot clones with transaction IDs and statuses",
		Long: `List recorded snapshot clones in transaction ID order, omitting records with empty clone names.
This lists recorded state, not database existence.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := config.ReadVCYamlAndSetupLogging(config.NewViperWithVCDefaults(), configPath)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			pool, err := statedb.NewPool(ctx, cfg.Database)
			if err != nil {
				return fmt.Errorf("failed to connect to database: %w", err)
			}
			defer pool.Close()
			states, err := statedb.NewSnapshotStateManager(pool, cfg.Database.Retry).List(ctx)
			if err != nil {
				return fmt.Errorf("failed to list snapshot database clones: %w", err)
			}
			return writeSnapshotClones(cmd.OutOrStdout(), states)
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Timeout for the listing operation")
	return cmd
}

// deleteCloneCMD creates the "db clone delete" command.
func deleteCloneCMD() *cobra.Command {
	var configPath, txID, cloneName string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   deleteCloneCommand,
		Short: "Delete the database clone of a CHECKPOINTED or ABORTED snapshot",
		Long: `Drop the database clone identified by exactly one of --tx-id or --name and clear the clone name from
its _snapshot record, keeping the record. The snapshot must be CHECKPOINTED or ABORTED, since
until then its clone may still be hashed. This can run while the committer is running.
Names must identify exactly one recorded snapshot clone, never an arbitrary database.
Retrying by --tx-id after a success or failure is safe. After successful deletion,
the cleared record no longer identifies its former clone name.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("tx-id") == cmd.Flags().Changed("name") ||
				(txID == "" && cloneName == "") {
				return errors.New("specify exactly one nonempty selector: --tx-id or --name")
			}
			cfg, _, err := config.ReadVCYamlAndSetupLogging(config.NewViperWithVCDefaults(), configPath)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			if cloneName != "" {
				err = statedb.DeleteSnapshotCloneByName(ctx, cfg.Database, cloneName)
			} else {
				err = statedb.DeleteSnapshotClone(ctx, cfg.Database, txID)
			}
			if err != nil {
				return fmt.Errorf("failed to delete the snapshot database clone: %w", err)
			}

			if cloneName != "" {
				cmd.Printf("Snapshot database clone %s deleted\n", cloneName)
			} else {
				cmd.Printf("Snapshot database clone for tx %s deleted\n", txID)
			}
			return nil
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	cmd.Flags().StringVar(&txID, "tx-id", "", "Transaction ID of the snapshot transaction")
	cmd.Flags().StringVar(&cloneName, "name", "", "Clone database name recorded in a snapshot record")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Timeout for the deletion operation")
	return cmd
}

// writeSnapshotClones writes a table omitting records with empty clone names.
func writeSnapshotClones(writer io.Writer, states []*committerpb.SnapshotState) error {
	out := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(out, "TX_ID\tCLONE_NAME\tSTATUS"); err != nil {
		return errors.Wrap(err, "failed to write snapshot clone list")
	}
	for _, state := range states {
		clone := state.CloneDatabase
		if clone == "" {
			continue
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", state.TxRef.TxId, clone, state.Status); err != nil {
			return errors.Wrap(err, "failed to write snapshot clone list")
		}
	}
	return errors.Wrap(out.Flush(), "failed to write snapshot clone list")
}
