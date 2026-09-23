/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/service/vc"
)

func main() {
	cmd := committerCMD()
	// On failure, Cobra prints the usage message and error string, so we only
	// need to exit with a non-0 status
	if cmd.Execute() != nil {
		os.Exit(1)
	}
}

func committerCMD() *cobra.Command {
	var snapshotPath, activationPath, verificationPath, configPath string
	cmd := &cobra.Command{
		Use:   cliutil.CommitterName,
		Short: fmt.Sprintf("Fabric-X %s.", cliutil.CommitterName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operations := 0
			for _, path := range []string{snapshotPath, activationPath, verificationPath} {
				if path != "" {
					operations++
				}
			}
			if operations > 1 {
				return fmt.Errorf("--init-from-snapshot, --activate-migration, and --verify-migration are mutually exclusive")
			}
			switch {
			case snapshotPath != "":
				return initFromSnapshot(cmd.Context(), cmd, configPath, snapshotPath)
			case activationPath != "":
				return activateMigration(cmd.Context(), cmd, configPath, activationPath)
			case verificationPath != "":
				return verifyMigration(cmd.Context(), cmd, configPath, verificationPath)
			default:
				return cmd.Help()
			}
		},
	}
	cmd.Flags().StringVar(&snapshotPath, "init-from-snapshot", "", "initialize VC state from a genesis-data file")
	cmd.Flags().StringVar(&activationPath, "activate-migration", "", "activate an already verified genesis-data migration")
	cmd.Flags().StringVar(&verificationPath, "verify-migration", "", "compare a genesis-data migration with live VC state")
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "set the validator-committer config file path")
	cmd.AddCommand(cliutil.VersionCmd())
	cmd.AddCommand(startCMD())
	cmd.AddCommand(healthcheckCMD())
	cmd.AddCommand(databaseInitializationCMD())
	return cmd
}

func verifyMigration(ctx context.Context, cmd *cobra.Command, configPath, snapshotPath string) error {
	conf, _, err := readConfig(vcService, configPath)
	if err != nil {
		return err
	}
	result, err := vc.VerifySnapshot(ctx, conf.(*vc.Config), snapshotPath)
	if err != nil {
		return err
	}
	cmd.Printf("Migration ID: %s\n", result.MigrationID)
	cmd.Printf("Migration status: %s\n", result.MigrationStatus)
	cmd.Printf("Source: channel %s block %d\n", result.SourceChannel, result.SourceBlockNumber)
	cmd.Printf("Target anchor: %d\n", result.TargetAnchor)
	cmd.Printf("Target configuration SHA-256: %s\n", result.TargetConfigSHA256)
	cmd.Printf("Namespace map SHA-256: %s\n", result.NamespaceMapSHA256)
	cmd.Printf("Target policies SHA-256: %s\n", result.TargetPolicySHA256)
	cmd.Printf("Public state: %d records SHA-256 %s\n", result.PublicStateCount, result.PublicStateSHA256)
	cmd.Printf("Transaction IDs: %d records SHA-256 %s\n", result.TransactionIDCount, result.TransactionIDSHA256)
	cmd.Println("Integrity: verified")
	return nil
}

func activateMigration(ctx context.Context, cmd *cobra.Command, configPath, snapshotPath string) error {
	conf, _, err := readConfig(vcService, configPath)
	if err != nil {
		return err
	}
	result, err := vc.ActivateSnapshot(ctx, conf.(*vc.Config), snapshotPath)
	if err != nil {
		return err
	}
	cmd.Printf("Migration ID: %s\n", result.MigrationID)
	cmd.Printf("Target anchor: %d\n", result.TargetAnchor)
	if result.AlreadyActive {
		cmd.Println("Status: already active")
	} else {
		cmd.Println("Status: active")
	}
	return nil
}

func initFromSnapshot(ctx context.Context, cmd *cobra.Command, configPath, snapshotPath string) error {
	conf, _, err := readConfig(vcService, configPath)
	if err != nil {
		return err
	}
	result, err := vc.InitFromSnapshot(ctx, conf.(*vc.Config), snapshotPath)
	if err != nil {
		return err
	}
	cmd.Printf("Migration ID: %s\n", result.MigrationID)
	cmd.Printf("Source: channel %s block %d\n", result.SourceChannel, result.SourceBlockNumber)
	cmd.Printf("Target anchor: %d\n", result.TargetAnchor)
	cmd.Printf("Imported: %d public records, %d transaction IDs\n", result.PublicStateCount, result.TransactionIDs)
	if result.AlreadyImported {
		cmd.Println("Status: already verified")
	} else {
		cmd.Println("Status: verified")
	}
	return nil
}
