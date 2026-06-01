/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package cliutil

import (
	"time"

	"github.com/spf13/cobra"
)

// SetDefaultFlags registers the --config flag on the given command.
func SetDefaultFlags(cmd *cobra.Command, configPath *string) {
	cmd.PersistentFlags().StringVarP(configPath, "config", "c", "", "set the config file path")
}

// SetDurationFlag registers a duration flag with the given name on the given command.
func SetDurationFlag(cmd *cobra.Command, name string, duration *time.Duration) {
	cmd.Flags().DurationVar(duration, name, 5*time.Minute, "Timeout for the initialization operation")
}
