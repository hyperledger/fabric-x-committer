/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package cliutil

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Service names and version.
const (
	CommitterVersion = "0.0.2"
	CommitterName    = "Committer"
)

// VersionCmd creates a version command.
func VersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "version",
		Short:        fmt.Sprintf("print %s version", CommitterName),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Printf("%s\n", FullCommitterVersion())
			return nil
		},
	}
}

// FullCommitterVersion returns the committer version string.
func FullCommitterVersion() string {
	return fmt.Sprintf("%s version %s %s/%s", CommitterName, CommitterVersion, runtime.GOOS, runtime.GOARCH)
}
