/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// Config describes the load generator metrics.
// It adds latency tracker to the common metrics configurations.
type Config struct {
	connection.ServerConfig `mapstructure:",squash" yaml:",inline"`
	Latency                 LatencyConfig `mapstructure:"latency" yaml:"latency"`
}
