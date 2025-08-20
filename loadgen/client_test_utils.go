/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package loadgen

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/loadgen/adapters"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

const defaultBlockSize = 500

func eventuallyMetrics(
	t *testing.T,
	m *metrics.PerfMetrics,
	condition func(m metrics.MetricState) bool,
) {
	t.Helper()
	if !assert.Eventually(t, func() bool {
		return condition(m.GetState())
	}, 10*time.Minute, 5*time.Second) {
		t.Fatalf("Metrics target was not achieved: %+v", m.GetState())
	}
}

// DefaultClientConf returns default config values for client testing.
func DefaultClientConf() *ClientConfig {
	return &ClientConfig{
		Server: connection.NewLocalHostServer(),
		Monitoring: metrics.Config{
			Config: defaultMonitoring(),
		},
		LoadProfile: &workload.Profile{
			Key:   workload.KeyProfile{Size: 32},
			Block: workload.BlockProfile{Size: defaultBlockSize},
			Transaction: workload.TransactionProfile{
				ReadWriteCount: workload.NewConstantDistribution(2),
				Policy: &workload.PolicyProfile{
					NamespacePolicies: map[string]*workload.Policy{
						workload.GeneratedNamespaceID: {
							Scheme: signature.Ecdsa,
							Seed:   10,
						},
						types.MetaNamespaceID: {
							Scheme: signature.Ecdsa,
							Seed:   11,
						},
					},
					ChannelID: "channel",
				},
			},
			Seed: 12345,
			// We use small number of workers to reduce the CPU load during tests.
			Workers: 1,
		},
		Stream: &workload.StreamOptions{
			RateLimit: &workload.LimiterConfig{InitialLimit: 1_000},
			// We set low values for the buffer and batch to reduce the CPU load during tests.
			BuffersSize: 1,
			GenBatch:    1,
		},
		Generate: adapters.Phases{
			Config:     true,
			Namespaces: true,
			Load:       true,
		},
	}
}

func defaultMonitoring() monitoring.Config {
	return monitoring.Config{
		Server: connection.NewLocalHostServer(),
	}
}
