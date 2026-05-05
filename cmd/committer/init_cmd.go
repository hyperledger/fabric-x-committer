/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/service/vc"
)

// initFromSnapshotCMD creates the "init-from-snapshot" subcommand. It reads
// a genesis-data file produced by the fxmigrate exporter and bootstraps the
// validator-committer's state database from it. The command is one-shot:
// it runs the import and exits, it does not start the VC service.
//
// The intended workflow is:
//
//  1. Operator runs the migration tool (fxmigrate) against a Fabric peer
//     snapshot to produce a genesis-data file.
//  2. Operator provisions a fresh database for the new Fabric-X deployment.
//  3. Operator runs `committer init-from-snapshot --config <vc.yaml>
//     --snapshot-data <genesis.bin>` against that fresh database.
//  4. Operator starts the committer normally (`committer start vc`); it
//     resumes processing at block N+1 where N is the snapshot's block height.
func initFromSnapshotCMD() *cobra.Command {
	var (
		configPath   string
		snapshotPath string
	)

	cmd := &cobra.Command{
		Use:   "init-from-snapshot",
		Short: "Initialise the VC state database from a Fabric peer snapshot.",
		Long: `init-from-snapshot bootstraps a fresh validator-committer state database
from a genesis-data file produced by the fxmigrate exporter.

The command is one-shot: it imports the snapshot and exits. The committer
must then be started normally with "committer start vc" to resume block
processing at the snapshot's block height + 1.

This command must be run against a fresh database. Re-running on a database
that already has the target namespace will fail rather than silently merging
state.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInitFromSnapshot(cmd, configPath, snapshotPath)
		},
	}

	cliutil.SetDefaultFlags(cmd, &configPath)
	cmd.Flags().StringVar(
		&snapshotPath,
		"snapshot-data",
		"",
		"Path to the genesis-data file produced by fxmigrate (required)",
	)
	if err := cmd.MarkFlagRequired("snapshot-data"); err != nil {
		// MarkFlagRequired only errors when the flag was never registered, which
		// would indicate a programming bug here rather than a user input issue.
		panic(err)
	}

	return cmd
}

func runInitFromSnapshot(cmd *cobra.Command, configPath, snapshotPath string) error {
	conf, err := readConfig(vcService, configPath)
	if err != nil {
		return err
	}
	vcConf, ok := conf.(*vc.Config)
	if !ok {
		return errors.Newf("init-from-snapshot requires a VC config; got %T", conf)
	}

	cmd.Printf("Initialising state database from snapshot: %s\n", snapshotPath)
	if err := vc.BootstrapFromSnapshot(cmd.Context(), vcConf.Database, snapshotPath); err != nil {
		return err
	}
	cmd.Println("Bootstrap complete. Start the committer with `committer start vc` to begin block processing.")
	return nil
}
