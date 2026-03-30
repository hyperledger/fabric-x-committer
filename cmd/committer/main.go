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
	cmd := &cobra.Command{
		Use:   config.CommitterName,
		Short: fmt.Sprintf("Fabric-X %s.", config.CommitterName),
	}
	cmd.AddCommand(config.VersionCmd())
	cmd.AddCommand(config.StartCMD())
	return cmd
}
