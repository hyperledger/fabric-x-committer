/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/loadgen"
	"github.com/hyperledger/fabric-x-committer/loadgen/adapters"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/query"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/broadcastdeliver"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

func TestReadConfigSidecar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		configFilePath string
		expectedConfig *sidecar.Config
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedConfig: &sidecar.Config{
			Server:     makeServer("localhost", 4001),
			Monitoring: makeMonitoring("localhost", 2114),
			Orderer: broadcastdeliver.Config{
				Connection: broadcastdeliver.ConnectionConfig{
					Endpoints: connection.NewOrdererEndpoints(0, "", makeServer("localhost", 7050)),
				},
				ChannelID: "mychannel",
			},
			Committer: sidecar.CoordinatorConfig{
				Endpoint: *makeEndpoint("localhost", 9001),
			},
			Ledger: sidecar.LedgerConfig{
				Path: "./ledger/",
			},
			LastCommittedBlockSetInterval: 3 * time.Second,
			WaitingTxsLimit:               100_000,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/sidecar.yaml",
		expectedConfig: &sidecar.Config{
			Server: &connection.ServerConfig{
				Endpoint: *makeEndpoint("", 4001),
				KeepAlive: &connection.ServerKeepAliveConfig{
					Params: &connection.ServerKeepAliveParamsConfig{
						Time:    300 * time.Second,
						Timeout: 600 * time.Second,
					},
					EnforcementPolicy: &connection.ServerKeepAliveEnforcementPolicyConfig{
						MinTime:             60 * time.Second,
						PermitWithoutStream: false,
					},
				},
			},
			Monitoring: makeMonitoring("", 2114),
			Orderer: broadcastdeliver.Config{
				Connection: broadcastdeliver.ConnectionConfig{
					Endpoints: connection.NewOrdererEndpoints(
						0, "", makeServer("ordering-service", 7050),
					),
				},
				ChannelID: "mychannel",
			},
			Committer: sidecar.CoordinatorConfig{
				Endpoint: *makeEndpoint("coordinator", 9001),
			},
			Ledger: sidecar.LedgerConfig{
				Path: "/root/sc/ledger",
			},
			LastCommittedBlockSetInterval: 5 * time.Second,
			WaitingTxsLimit:               20_000_000,
		},
	}}
	for _, test := range tests {
		tt := test
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithSidecarDefaults()
			c, err := ReadSidecarYamlAndSetupLogging(v, tt.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tt.expectedConfig, c)
		})
	}
}

func TestReadConfigCoordinator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		configFilePath string
		expectedConfig *coordinator.Config
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedConfig: &coordinator.Config{
			Server:     makeServer("localhost", 9001),
			Monitoring: makeMonitoring("localhost", 2119),
			DependencyGraphConfig: &coordinator.DependencyGraphConfig{
				NumOfLocalDepConstructors:       1,
				WaitingTxsLimit:                 100_000,
				NumOfWorkersForGlobalDepManager: 1,
			},
			ChannelBufferSizePerGoroutine: 10,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/coordinator.yaml",
		expectedConfig: &coordinator.Config{
			Server:     makeServer("", 9001),
			Monitoring: makeMonitoring("", 2119),
			VerifierConfig: connection.ClientConfig{
				Endpoints: []*connection.Endpoint{makeEndpoint("signature-verifier", 5001)},
			},
			ValidatorCommitterConfig: connection.ClientConfig{
				Endpoints: []*connection.Endpoint{makeEndpoint("validator-persister", 6001)},
			},
			DependencyGraphConfig: &coordinator.DependencyGraphConfig{
				NumOfLocalDepConstructors:       1,
				WaitingTxsLimit:                 10_000,
				NumOfWorkersForGlobalDepManager: 1,
			},
			ChannelBufferSizePerGoroutine: 10,
		},
	}}

	for _, test := range tests {
		tt := test
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithCoordinatorDefaults()
			c, err := ReadCoordinatorYamlAndSetupLogging(v, tt.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tt.expectedConfig, c)
		})
	}
}

func TestReadConfigVC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		configFilePath string
		expectedConfig *vc.Config
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedConfig: &vc.Config{
			Server:     makeServer("localhost", 6001),
			Monitoring: makeMonitoring("localhost", 2116),
			Database:   defaultDBConfig(),
			ResourceLimits: &vc.ResourceLimitsConfig{
				MaxWorkersForPreparer:             1,
				MaxWorkersForValidator:            1,
				MaxWorkersForCommitter:            20,
				MinTransactionBatchSize:           1,
				TimeoutForMinTransactionBatchSize: 5 * time.Second,
			},
		},
	}, {
		name:           "sample",
		configFilePath: "samples/vcservice.yaml",
		expectedConfig: &vc.Config{
			Server:     makeServer("", 6001),
			Monitoring: makeMonitoring("", 2116),
			Database:   defaultSampleDBConfig(),
			ResourceLimits: &vc.ResourceLimitsConfig{
				MaxWorkersForPreparer:             1,
				MaxWorkersForValidator:            1,
				MaxWorkersForCommitter:            20,
				MinTransactionBatchSize:           1,
				TimeoutForMinTransactionBatchSize: 2 * time.Second,
			},
		},
	}}

	for _, test := range tests {
		tt := test
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithVCDefaults()
			c, err := ReadVCYamlAndSetupLogging(v, tt.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tt.expectedConfig, c)
		})
	}
}

func TestReadConfigVerifier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		configFilePath string
		expectedConfig *verifier.Config
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedConfig: &verifier.Config{
			Server:     makeServer("localhost", 5001),
			Monitoring: makeMonitoring("localhost", 2115),
			ParallelExecutor: verifier.ExecutorConfig{
				Parallelism:       4,
				BatchSizeCutoff:   50,
				BatchTimeCutoff:   500 * time.Millisecond,
				ChannelBufferSize: 50,
			},
		},
	}, {
		name:           "sample",
		configFilePath: "samples/sigservice.yaml",
		expectedConfig: &verifier.Config{
			Server:     makeServer("", 5001),
			Monitoring: makeMonitoring("", 2115),
			ParallelExecutor: verifier.ExecutorConfig{
				BatchSizeCutoff:   50,
				BatchTimeCutoff:   10 * time.Millisecond,
				ChannelBufferSize: 50,
				Parallelism:       40,
			},
		},
	}}

	for _, test := range tests {
		tt := test
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithVerifierDefaults()
			c, err := ReadVerifierYamlAndSetupLogging(v, tt.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tt.expectedConfig, c)
		})
	}
}

func TestReadConfigQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		configFilePath string
		expectedConfig *query.Config
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedConfig: &query.Config{
			Server:                makeServer("localhost", 7001),
			Monitoring:            makeMonitoring("localhost", 2117),
			Database:              defaultDBConfig(),
			MinBatchKeys:          1024,
			MaxBatchWait:          100 * time.Millisecond,
			ViewAggregationWindow: 100 * time.Millisecond,
			MaxAggregatedViews:    1024,
			MaxViewTimeout:        10 * time.Second,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/queryservice.yaml",
		expectedConfig: &query.Config{
			Server:                makeServer("", 7001),
			Monitoring:            makeMonitoring("", 2117),
			Database:              defaultSampleDBConfig(),
			MinBatchKeys:          1024,
			MaxBatchWait:          100 * time.Millisecond,
			ViewAggregationWindow: 100 * time.Millisecond,
			MaxAggregatedViews:    1024,
			MaxViewTimeout:        10 * time.Second,
		},
	}}

	for _, test := range tests {
		tt := test
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithQueryDefaults()
			c, err := ReadQueryYamlAndSetupLogging(v, tt.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tt.expectedConfig, c)
		})
	}
}

func TestReadConfigLoadGen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		configFilePath string
		expectedConfig *loadgen.ClientConfig
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedConfig: &loadgen.ClientConfig{
			Server: makeServer("localhost", 8001),
			Monitoring: metrics.Config{
				Config: makeMonitoring("localhost", 2118),
			},
		},
	}, {
		name:           "sample",
		configFilePath: "samples/loadgen.yaml",
		expectedConfig: &loadgen.ClientConfig{
			Server: makeServer("", 8001),
			Monitoring: metrics.Config{
				Config: makeMonitoring("", 2118),
				Latency: metrics.LatencyConfig{
					SamplerConfig: metrics.SamplerConfig{
						Portion: 0.01,
					},
					BucketConfig: metrics.BucketConfig{
						Distribution: "uniform",
						MaxLatency:   5 * time.Second,
						BucketCount:  1_000,
					},
				},
			},
			Adapter: adapters.AdapterConfig{
				OrdererClient: &adapters.OrdererClientConfig{
					SidecarEndpoint: makeEndpoint("sidecar", 4001),
					Orderer: broadcastdeliver.Config{
						Connection: broadcastdeliver.ConnectionConfig{
							Endpoints: connection.NewOrdererEndpoints(
								0, "", makeServer("ordering-service", 7050),
							),
						},
						ChannelID:     "mychannel",
						ConsensusType: "BFT",
					},
					BroadcastParallelism: 1,
				},
			},
			LoadProfile: &workload.Profile{
				Key:   workload.KeyProfile{Size: 32},
				Block: workload.BlockProfile{Size: 500},
				Transaction: workload.TransactionProfile{
					ReadWriteCount: workload.NewConstantDistribution(2),
					Policy: &workload.PolicyProfile{
						NamespacePolicies: map[string]*workload.Policy{
							workload.GeneratedNamespaceID: {
								Scheme: "ECDSA", Seed: 10,
							},
							types.MetaNamespaceID: {
								Scheme: "ECDSA", Seed: 11,
							},
						},
						OrdererEndpoints: []*connection.OrdererEndpoint{{
							ID:       0,
							MspID:    "org",
							API:      []string{"broadcast", "deliver"},
							Endpoint: *makeEndpoint("ordering-service", 7050),
						}},
					},
				},
				Conflicts: workload.ConflictProfile{
					InvalidSignatures: 0.1,
				},
				Seed:    12345,
				Workers: 1,
			},
			Stream: &workload.StreamOptions{
				RateLimit: &workload.LimiterConfig{
					Endpoint:     *makeEndpoint("", 6997),
					InitialLimit: 10_000,
				},
				BuffersSize: 10,
				GenBatch:    10,
			},
			Generate: adapters.Phases{
				Namespaces: true,
				Load:       true,
			},
			Limit: &adapters.GenerateLimit{
				Transactions: 50_000,
			},
		},
	}}

	for _, test := range tests {
		tt := test
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithLoadGenDefaults()
			c, err := ReadLoadGenYamlAndSetupLogging(v, tt.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tt.expectedConfig, c)
		})
	}
}

func defaultDBConfig() *vc.DatabaseConfig {
	return &vc.DatabaseConfig{
		Endpoints:      []*connection.Endpoint{makeEndpoint("localhost", 5433)},
		Username:       "yugabyte",
		Password:       "yugabyte",
		Database:       "yugabyte",
		MaxConnections: 20,
		MinConnections: 1,
		Retry: &connection.RetryProfile{
			MaxElapsedTime: 10 * time.Minute,
		},
	}
}

func defaultSampleDBConfig() *vc.DatabaseConfig {
	return &vc.DatabaseConfig{
		Endpoints:      []*connection.Endpoint{makeEndpoint("db", 5433)},
		Username:       "yugabyte",
		Password:       "yugabyte",
		Database:       "yugabyte",
		MaxConnections: 10,
		MinConnections: 5,
		LoadBalance:    false,
		Retry: &connection.RetryProfile{
			InitialInterval:     500 * time.Millisecond,
			RandomizationFactor: 0.5,
			Multiplier:          1.5,
			MaxInterval:         60 * time.Second,
			MaxElapsedTime:      15 * time.Minute,
		},
	}
}

func makeEndpoint(host string, port int) *connection.Endpoint {
	return &connection.Endpoint{
		Host: host,
		Port: port,
	}
}

func makeServer(host string, port int) *connection.ServerConfig {
	return &connection.ServerConfig{
		Endpoint: *makeEndpoint(host, port),
	}
}

func makeMonitoring(host string, port int) monitoring.Config {
	return monitoring.Config{Server: makeServer(host, port)}
}

func emptyConfig(t *testing.T) string {
	t.Helper()
	configPath := filepath.Clean(path.Join(t.TempDir(), "empty.yaml"))
	require.NoError(t, os.WriteFile(configPath, []byte{}, 0o660))
	return configPath
}
