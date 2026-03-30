/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
)

const (
	sidecarService     = "sidecar"
	coordinatorService = "coordinator"
	vcService          = "vc"
	verifierService    = "verifier"
	queryService       = "query"
)

var serviceNames = map[string]string{
	sidecarService:     "Sidecar",
	coordinatorService: "Coordinator",
	vcService:          "Validator-Committer",
	verifierService:    "Verifier",
	queryService:       "Query-Service",
}

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
		Use:   cliutil.CommitterName,
		Short: fmt.Sprintf("Fabric-X %s.", cliutil.CommitterName),
	}
	cmd.AddCommand(cliutil.VersionCmd())
	cmd.AddCommand(startCMD())
	return cmd
}
