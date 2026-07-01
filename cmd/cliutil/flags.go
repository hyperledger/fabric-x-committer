/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package cliutil

import (
	"github.com/spf13/cobra"
)

// SetDefaultFlags registers the --config flag on the given command.
func SetDefaultFlags(cmd *cobra.Command, configPath *string) {
	cmd.PersistentFlags().StringVarP(configPath, "config", "c", "", "set the config file path")
}
