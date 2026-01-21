/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"time"

	"github.com/spf13/viper"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

// NewViperWithCoordinatorDefaults returns a viper instance with the coordinator default values.
func NewViperWithCoordinatorDefaults() *viper.Viper {
	v := NewViperWithServiceDefault(9001, 2119)
	v.SetDefault("dependency-graph.num-of-local-dep-constructors", 1)
	v.SetDefault("dependency-graph.waiting-txs-limit", 100_000)
	v.SetDefault("per-channel-buffer-size-per-goroutine", 10)
	return v
}

// NewViperWithSidecarDefaults returns a viper instance with the sidecar default values.
func NewViperWithSidecarDefaults() *viper.Viper {
	v := NewViperWithServiceDefault(4001, 2114)
	v.SetDefault("orderer.channel-id", "mychannel")
	v.SetDefault("committer.endpoint", "localhost:9001")
	v.SetDefault("ledger.path", "./ledger/")
	v.SetDefault("notification.max-timeout", "1m")
	v.SetDefault("last-committed-block-set-interval", "3s")
	v.SetDefault("waiting-txs-limit", 100_000)
	v.SetDefault("channel-buffer-size", 100)
	return v
}

// NewViperWithVerifierDefaults returns a viper instance with the verifier default values.
func NewViperWithVerifierDefaults() *viper.Viper {
	v := NewViperWithServiceDefault(5001, 2115)
	v.SetDefault("parallel-executor.parallelism", 4)
	v.SetDefault("parallel-executor.batch-time-cutoff", "500ms")
	v.SetDefault("parallel-executor.batch-size-cutoff", 50)
	v.SetDefault("parallel-executor.channel-buffer-size", 50)
	return v
}

// NewViperWithVCDefaults returns a viper instance with the VC default values.
func NewViperWithVCDefaults() *viper.Viper {
	v := NewViperWithServiceDefault(6001, 2116)
	defaultDBFlags(v)
	// defaults for ResourceLimitsConfig
	limitPrefix := "resource-limits."
	v.SetDefault(limitPrefix+"max-workers-for-preparer", 1)
	v.SetDefault(limitPrefix+"max-workers-for-validator", 1)
	v.SetDefault(limitPrefix+"max-workers-for-committer", 20)
	v.SetDefault(limitPrefix+"min-transaction-batch-size", 1)
	v.SetDefault(limitPrefix+"timeout-for-min-transaction-batch-size", 5*time.Second)
	return v
}

// NewViperWithQueryDefaults returns a viper instance with the query-service default values.
func NewViperWithQueryDefaults() *viper.Viper {
	v := NewViperWithServiceDefault(7001, 2117)
	defaultDBFlags(v)
	v.SetDefault("min-batch-keys", 1024)
	v.SetDefault("max-batch-wait", 100*time.Millisecond)
	v.SetDefault("view-aggregation-window", 100*time.Millisecond)
	v.SetDefault("max-aggregated-views", 1024)
	v.SetDefault("max-view-timeout", 10*time.Second)
	v.SetDefault("max-request-keys", 0) // 0 means no limit
	return v
}

// NewViperWithLoadGenDefaults returns a viper instance with the load generator default values.
func NewViperWithLoadGenDefaults() *viper.Viper {
	return NewViperWithServiceDefault(8001, 2118)
}

// NewViperWithServiceDefault returns a viper instance with a service default values.
func NewViperWithServiceDefault(servicePort, monitoringPort int) *viper.Viper {
	v := NewViperWithLoggingDefault()
	v.SetDefault("server.endpoint", &connection.Endpoint{Host: "localhost", Port: servicePort})
	v.SetDefault("monitoring.server.endpoint", &connection.Endpoint{Host: "localhost", Port: monitoringPort})
	// Rate limiting disabled by default (0 = disabled)
	v.SetDefault("server.rate-limit", &connection.RateLimitConfig{})
	v.SetDefault("monitoring.server.rate-limit", &connection.RateLimitConfig{})
	return v
}

// NewViperWithLoggingDefault returns a viper instance with the logging default values.
func NewViperWithLoggingDefault() *viper.Viper {
	v := viper.New()
	v.SetDefault("logging.development", "false")
	v.SetDefault("logging.enabled", "true")
	v.SetDefault("logging.level", logging.Info)
	return v
}

// defaultDBFlags sets the default DB parameters.
func defaultDBFlags(v *viper.Viper) {
	prefix := "database."
	v.SetDefault(prefix+"endpoints", []*connection.Endpoint{{Host: "localhost", Port: 5433}})
	v.SetDefault(prefix+"username", "yugabyte")
	v.SetDefault(prefix+"password", "yugabyte")
	v.SetDefault(prefix+"database", "yugabyte")
	v.SetDefault(prefix+"max-connections", 20)
	v.SetDefault(prefix+"min-connections", 1)
	// We allow 10 minutes by default for a DB recovery.
	v.SetDefault(prefix+"retry.max-elapsed-time", 10*time.Minute)
}
