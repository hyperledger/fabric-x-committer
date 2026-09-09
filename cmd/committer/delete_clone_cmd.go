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

const deleteCloneCommand = "delete-clone"

// deleteCloneCMD creates the "delete-clone" command.
func deleteCloneCMD() *cobra.Command {
	var configPath, txID string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   deleteCloneCommand,
		Short: "Delete the database clone of a CHECKPOINTED or ABORTED snapshot",
		Long: `Drop the database clone of the snapshot transaction --tx-id and clear the clone name from
its _snapshot record, keeping the record. The snapshot must be CHECKPOINTED or ABORTED, since
until then its clone may still be hashed. This can run while the committer is running.
Running it again after a success, or after a failure, is safe.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := config.ReadVCYamlAndSetupLogging(config.NewViperWithVCDefaults(), configPath)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			if err := statedb.DeleteSnapshotClone(ctx, cfg.Database, txID); err != nil {
				return errors.Wrap(err, "failed to delete the snapshot database clone")
			}

			cmd.Printf("Snapshot database clone for tx %s deleted\n", txID)
			return nil
		},
	}
	cliutil.SetDefaultFlags(cmd, &configPath)
	cmd.Flags().StringVar(&txID, "tx-id", "", "Transaction ID of the snapshot transaction")
	_ = cmd.MarkFlagRequired("tx-id")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Timeout for the deletion operation")
	return cmd
}
