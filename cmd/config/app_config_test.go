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

	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen"
	"github.com/hyperledger/fabric-x-committer/loadgen/adapters"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/query"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/dbconn"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const artifactsPath = "/root/artifacts"

func TestReadConfigSidecar(t *testing.T) {
	t.Parallel()
	sidecarTLSCreds := test.NewServiceTLSConfig(artifactsPath, "sidecar", connection.MutualTLSMode)
	tests := []struct {
		name           string
		configFilePath string
		expectedConfig *sidecar.Config
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedConfig: &sidecar.Config{
			Server: &connection.ServerConfig{
				Endpoint:             *newEndpoint(connection.DefaultHost, sidecar.DefaultServerPort),
				MaxConcurrentStreams: sidecar.DefaultMaxConcurrentStreams,
			},
			Monitoring: newServerConfig(connection.DefaultHost, sidecar.DefaultMonitoringPort),
			Committer: &connection.ClientConfig{
				Endpoint: newEndpoint(connection.DefaultHost, coordinator.DefaultServerPort),
			},
			Orderer: ordererdial.Config{
				SuspicionGracePeriodPerBlock: time.Second,
			},
			Ledger: sidecar.LedgerConfig{
				Path: "./ledger/",
			},
			Notification: sidecar.NotificationServiceConfig{
				MaxTimeout:         sidecar.DefaultNotificationMaxTimeout,
				MaxActiveTxIDs:     sidecar.DefaultMaxActiveTxIDs,
				MaxTxIDsPerRequest: sidecar.DefaultMaxTxIDsPerRequest,
			},
			LastCommittedBlockSetInterval: sidecar.DefaultLastCommittedBlockSetInterval,
			WaitingTxsLimit:               sidecar.DefaultWaitingTxsLimit,
			ChannelBufferSize:             sidecar.DefaultBufferSize,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/sidecar.yaml",
		expectedConfig: &sidecar.Config{
			Server: &connection.ServerConfig{
				Endpoint: *newEndpoint("", 4001),
				TLS:      sidecarTLSCreds,
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
				MaxConcurrentStreams: 10,
			},
			Monitoring: newServerConfigWithDefaultTLS("sidecar", 2114),
			Orderer: ordererdial.Config{
				FaultToleranceLevel:        ordererdial.BFT,
				LatestKnownConfigBlockPath: "/root/artifacts/config-block.pb.bin",
				Identity:                   newIdentityConfig(),
				TLS: ordererdial.TLSConfig{
					Mode:     sidecarTLSCreds.Mode,
					KeyPath:  sidecarTLSCreds.KeyPath,
					CertPath: sidecarTLSCreds.CertPath,
				},
				SuspicionGracePeriodPerBlock: time.Second,
			},
			Committer: newClientConfigWithDefaultTLS("coordinator", "sidecar", 9001),
			Ledger: sidecar.LedgerConfig{
				Path:         "/root/sc/ledger",
				SyncInterval: 100,
			},
			Notification: sidecar.NotificationServiceConfig{
				MaxTimeout:         10 * time.Minute,
				MaxActiveTxIDs:     100_000,
				MaxTxIDsPerRequest: 1000,
			},
			LastCommittedBlockSetInterval: sidecar.DefaultLastCommittedBlockSetInterval,
			WaitingTxsLimit:               20_000_000,
			ChannelBufferSize:             sidecar.DefaultBufferSize,
		},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithSidecarDefaults()
			c, err := ReadSidecarYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedConfig, c)
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
			Server:     newServerConfig(connection.DefaultHost, coordinator.DefaultServerPort),
			Monitoring: newServerConfig(connection.DefaultHost, coordinator.DefaultMonitoringPort),
			DependencyGraph: &coordinator.DependencyGraphConfig{
				NumOfLocalDepConstructors: coordinator.DefaultNumOfLocalDepConstructors,
				WaitingTxsLimit:           coordinator.DefaultWaitingTxsLimit,
			},
			ChannelBufferSizePerGoroutine: coordinator.DefaultChannelBufferSizePerGoroutine,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/coordinator.yaml",
		expectedConfig: &coordinator.Config{
			Server:             newServerConfigWithDefaultTLS("coordinator", 9001),
			Monitoring:         newServerConfigWithDefaultTLS("coordinator", 2119),
			Verifier:           newMultiClientConfigWithDefaultTLS("verifier", "coordinator", 5001),
			ValidatorCommitter: newMultiClientConfigWithDefaultTLS("vc", "coordinator", 6001),
			DependencyGraph: &coordinator.DependencyGraphConfig{
				NumOfLocalDepConstructors: coordinator.DefaultNumOfLocalDepConstructors,
				WaitingTxsLimit:           coordinator.DefaultWaitingTxsLimit,
			},
			ChannelBufferSizePerGoroutine: coordinator.DefaultChannelBufferSizePerGoroutine,
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithCoordinatorDefaults()
			c, err := ReadCoordinatorYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedConfig, c)
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
			Server:     newServerConfig(connection.DefaultHost, vc.DefaultServerPort),
			Monitoring: newServerConfig(connection.DefaultHost, vc.DefaultMonitoringPort),
			Database:   defaultDBConfig(),
			ResourceLimits: &vc.ResourceLimitsConfig{
				MaxWorkersForPreparer:             vc.DefaultMaxWorkersForPreparer,
				MaxWorkersForValidator:            vc.DefaultMaxWorkersForValidator,
				MaxWorkersForCommitter:            vc.DefaultMaxWorkersForCommitter,
				MinTransactionBatchSize:           vc.DefaultMinTransactionBatchSize,
				TimeoutForMinTransactionBatchSize: vc.DefaultTimeoutForMinBatchSize,
			},
		},
	}, {
		name:           "sample",
		configFilePath: "samples/vc.yaml",
		expectedConfig: &vc.Config{
			Server:     newServerConfigWithDefaultTLS("vc", 6001),
			Monitoring: newServerConfigWithDefaultTLS("vc", 2116),
			Database:   defaultSampleDBConfig(),
			ResourceLimits: &vc.ResourceLimitsConfig{
				MaxWorkersForPreparer:             vc.DefaultMaxWorkersForPreparer,
				MaxWorkersForValidator:            vc.DefaultMaxWorkersForValidator,
				MaxWorkersForCommitter:            vc.DefaultMaxWorkersForCommitter,
				MinTransactionBatchSize:           vc.DefaultMinTransactionBatchSize,
				TimeoutForMinTransactionBatchSize: 2 * time.Second,
			},
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithVCDefaults()
			c, err := ReadVCYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedConfig, c)
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
			Server:     newServerConfig(connection.DefaultHost, verifier.DefaultServerPort),
			Monitoring: newServerConfig(connection.DefaultHost, verifier.DefaultMonitoringPort),
			ParallelExecutor: verifier.ExecutorConfig{
				Parallelism:       verifier.DefaultParallelism,
				BatchSizeCutoff:   verifier.DefaultBatchSizeCutoff,
				BatchTimeCutoff:   verifier.DefaultBatchTimeCutoff,
				ChannelBufferSize: verifier.DefaultChannelBufferSize,
			},
		},
	}, {
		name:           "sample",
		configFilePath: "samples/verifier.yaml",
		expectedConfig: &verifier.Config{
			Server:     newServerConfigWithDefaultTLS("verifier", 5001),
			Monitoring: newServerConfigWithDefaultTLS("verifier", 2115),
			ParallelExecutor: verifier.ExecutorConfig{
				BatchSizeCutoff:   verifier.DefaultBatchSizeCutoff,
				BatchTimeCutoff:   10 * time.Millisecond,
				ChannelBufferSize: verifier.DefaultChannelBufferSize,
				Parallelism:       40,
			},
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithVerifierDefaults()
			c, err := ReadVerifierYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedConfig, c)
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
			Server: &connection.ServerConfig{
				Endpoint: *newEndpoint(connection.DefaultHost, query.DefaultServerPort),
				RateLimit: connection.RateLimitConfig{
					RequestsPerSecond: query.DefaultRequestsPerSecond,
					Burst:             query.DefaultBurst,
				},
			},
			Monitoring:            newServerConfig(connection.DefaultHost, query.DefaultMonitoringPort),
			Database:              defaultDBConfig(),
			MinBatchKeys:          query.DefaultMinBatchKeys,
			MaxBatchWait:          query.DefaultMaxBatchWait,
			ViewAggregationWindow: query.DefaultViewAggregationWindow,
			MaxAggregatedViews:    query.DefaultMaxAggregatedViews,
			MaxActiveViews:        query.DefaultMaxActiveViews,
			MaxViewTimeout:        query.DefaultMaxViewTimeout,
			MaxRequestKeys:        query.DefaultMaxRequestKeys,
			TLSRefreshInterval:    query.DefaultTLSRefreshInterval,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/query.yaml",
		expectedConfig: &query.Config{
			Server:                newServerConfigWithDefaultTLS("query", 7001),
			Monitoring:            newServerConfigWithDefaultTLS("query", 2117),
			Database:              defaultSampleDBConfig(),
			MinBatchKeys:          query.DefaultMinBatchKeys,
			MaxBatchWait:          query.DefaultMaxBatchWait,
			ViewAggregationWindow: query.DefaultViewAggregationWindow,
			MaxAggregatedViews:    query.DefaultMaxAggregatedViews,
			MaxActiveViews:        query.DefaultMaxActiveViews,
			MaxViewTimeout:        query.DefaultMaxViewTimeout,
			MaxRequestKeys:        query.DefaultMaxRequestKeys,
			TLSRefreshInterval:    query.DefaultTLSRefreshInterval,
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithQueryDefaults()
			c, err := ReadQueryYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedConfig, c)
		})
	}
}

func TestReadConfigLoadGen(t *testing.T) {
	t.Parallel()
	loadgenTLSCreds := test.NewServiceTLSConfig(artifactsPath, "loadgen", connection.MutualTLSMode)
	tests := []struct {
		name           string
		configFilePath string
		expectedConfig *loadgen.ClientConfig
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedConfig: &loadgen.ClientConfig{
			Server: newServerConfig(connection.DefaultHost, loadgen.DefaultServerPort),
			Monitoring: metrics.Config{
				ServerConfig: *newServerConfig(connection.DefaultHost, loadgen.DefaultMonitoringPort),
			},
		},
	}, {
		name:           "sample",
		configFilePath: "samples/loadgen.yaml",
		expectedConfig: &loadgen.ClientConfig{
			Server:     newServerConfigWithDefaultTLS("loadgen", 8001),
			HTTPServer: newServerConfig("", 6997),
			Monitoring: metrics.Config{
				ServerConfig: *newServerConfigWithDefaultTLS("loadgen", 2118),
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
					SidecarClient: newClientConfigWithDefaultTLS("sidecar", "loadgen", 4001),
					Orderer: ordererdial.Config{
						FaultToleranceLevel:        ordererdial.BFT,
						LatestKnownConfigBlockPath: "/root/artifacts/config-block.pb.bin",
						Identity:                   newIdentityConfig(),
						TLS: ordererdial.TLSConfig{
							Mode:     loadgenTLSCreds.Mode,
							KeyPath:  loadgenTLSCreds.KeyPath,
							CertPath: loadgenTLSCreds.CertPath,
						},
					},
					BroadcastParallelism: 1,
				},
			},
			LoadProfile: &workload.Profile{
				Key: workload.KeyProfile{Size: 32},
				Block: workload.BlockProfile{
					MaxSize:       500,
					MinSize:       10,
					PreferredRate: time.Second,
				},
				Transaction: workload.TransactionProfile{
					ReadWriteCount: workload.NewConstantDistribution(2),
				},
				Policy: workload.PolicyProfile{
					ChannelID: "mychannel",
					NamespacePolicies: map[string]*workload.Policy{
						workload.DefaultGeneratedNamespaceID: {Scheme: workload.PolicySchemeMSP},
						"1":                                  {Scheme: signature.Ecdsa, Seed: 10},
					},
					OrdererEndpoints: []*commontypes.OrdererEndpoint{{
						ID:   0,
						Host: "orderer",
						Port: 7050,
						API:  []string{commontypes.Broadcast, commontypes.Deliver},
					}},
					PeerOrganizationCount: 2,
					ArtifactsPath:         "/root/artifacts",
				},
				Conflicts: workload.ConflictProfile{
					InvalidSignatures: 0.1,
				},
				Seed:    12345,
				Workers: 1,
			},
			Stream: &workload.StreamOptions{
				RateLimit:   10_000,
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

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewViperWithLoadGenDefaults()
			c, err := ReadLoadGenYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedConfig, c)
		})
	}
}

func defaultDBConfig() *vc.DatabaseConfig {
	return &vc.DatabaseConfig{
		Endpoints:      []*connection.Endpoint{newEndpoint(connection.DefaultHost, vc.DefaultDatabaseEndpointPort)},
		Database:       vc.DefaultDatabaseName,
		MaxConnections: vc.DefaultDatabaseMaxConnections,
		MinConnections: vc.DefaultDatabaseMinConnections,
		Retry: &retry.Profile{
			MaxElapsedTime: vc.DefaultDatabaseRetryMaxElapsedTime,
		},
	}
}

func defaultSampleDBConfig() *vc.DatabaseConfig {
	return &vc.DatabaseConfig{
		Endpoints: []*connection.Endpoint{newEndpoint("db", 5433)},
		Username:  "yugabyte",
		Password:  "yugabyte",
		Database:  "yugabyte",
		TLS: dbconn.DatabaseTLSConfig{
			Mode:       connection.OneSideTLSMode,
			CACertPath: filepath.Join(artifactsPath, test.OrgRootCA),
		},
		MaxConnections: 10,
		MinConnections: 5,
		LoadBalance:    false,
		Retry: &retry.Profile{
			InitialInterval:     500 * time.Millisecond,
			RandomizationFactor: 0.5,
			Multiplier:          1.5,
			MaxInterval:         60 * time.Second,
			MaxElapsedTime:      15 * time.Minute,
		},
	}
}

func newClientConfigWithDefaultTLS(host, fromService string, port int) *connection.ClientConfig {
	return &connection.ClientConfig{
		Endpoint: newEndpoint(host, port),
		TLS:      test.NewServiceTLSConfig(artifactsPath, fromService, connection.MutualTLSMode),
	}
}

func newMultiClientConfigWithDefaultTLS(host, fromService string, port int) connection.MultiClientConfig {
	return connection.MultiClientConfig{
		Endpoints: []*connection.Endpoint{
			newEndpoint(host, port),
		},
		TLS: test.NewServiceTLSConfig(artifactsPath, fromService, connection.MutualTLSMode),
	}
}

func newServerConfigWithDefaultTLS(serviceName string, port int) *connection.ServerConfig {
	return &connection.ServerConfig{
		Endpoint: *newEndpoint("", port),
		TLS:      test.NewServiceTLSConfig(artifactsPath, serviceName, connection.MutualTLSMode),
	}
}

func newServerConfig(host string, port int) *connection.ServerConfig {
	return &connection.ServerConfig{
		Endpoint: *newEndpoint(host, port),
	}
}

func newEndpoint(host string, port int) *connection.Endpoint {
	return &connection.Endpoint{
		Host: host,
		Port: port,
	}
}

func newIdentityConfig() *ordererdial.IdentityConfig {
	return &ordererdial.IdentityConfig{
		MspID:  "peer-org-0",
		MSPDir: "/root/artifacts/peerOrganizations/peer-org-0.com/users/client@peer-org-0.com/msp",
		BCCSP: &factory.FactoryOpts{
			Default: "SW",
			SW: &factory.SwOpts{
				Hash:     "SHA2",
				Security: 256,
			},
		},
	}
}

func emptyConfig(t *testing.T) string {
	t.Helper()
	configPath := filepath.Clean(path.Join(t.TempDir(), "empty.yaml"))
	require.NoError(t, os.WriteFile(configPath, []byte{}, 0o660))
	return configPath
}

func TestViperDefaultsAreComplete(t *testing.T) {
	t.Parallel()

	t.Run("sidecar", func(t *testing.T) {
		t.Parallel()
		v := NewViperWithSidecarDefaults()
		c := &sidecar.Config{}
		require.NoError(t, unmarshal(v, c))
	})

	t.Run("coordinator", func(t *testing.T) {
		t.Parallel()
		v := NewViperWithCoordinatorDefaults()
		c := &coordinator.Config{}
		require.NoError(t, unmarshal(v, c))
	})

	t.Run("vc", func(t *testing.T) {
		t.Parallel()
		v := NewViperWithVCDefaults()
		c := &vc.Config{}
		require.NoError(t, unmarshal(v, c))
	})

	t.Run("verifier", func(t *testing.T) {
		t.Parallel()
		v := NewViperWithVerifierDefaults()
		c := &verifier.Config{}
		require.NoError(t, unmarshal(v, c))
	})

	t.Run("query", func(t *testing.T) {
		t.Parallel()
		v := NewViperWithQueryDefaults()
		c := &query.Config{}
		require.NoError(t, unmarshal(v, c))
	})
}
