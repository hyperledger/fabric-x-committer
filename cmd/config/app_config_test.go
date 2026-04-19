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
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const artifactsPath = "/root/artifacts"

func TestReadConfigSidecar(t *testing.T) {
	t.Parallel()
	sidecarTLSCreds := test.NewServiceTLSConfig(artifactsPath, "sidecar", connection.MutualTLSMode)
	tests := []struct {
		name                  string
		configFilePath        string
		expectedServiceConfig *sidecar.Config
		expectedServerConfig  *serve.Config
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedServerConfig: &serve.Config{
			GRPC: serve.ServerConfig{
				Endpoint:             *newEndpoint(connection.DefaultHost, sidecar.DefaultServerPort),
				MaxConcurrentStreams: sidecar.DefaultMaxConcurrentStreams,
			},
			Monitoring: *newServerConfig(connection.DefaultHost, sidecar.DefaultMonitoringPort),
		},
		expectedServiceConfig: &sidecar.Config{
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
		expectedServerConfig: &serve.Config{
			GRPC: serve.ServerConfig{
				Endpoint: *newEndpoint("", 4001),
				TLS:      sidecarTLSCreds,
				KeepAlive: &serve.ServerKeepAliveConfig{
					Params: &serve.ServerKeepAliveParamsConfig{
						Time:    300 * time.Second,
						Timeout: 600 * time.Second,
					},
					EnforcementPolicy: &serve.ServerKeepAliveEnforcementPolicyConfig{
						MinTime:             60 * time.Second,
						PermitWithoutStream: false,
					},
				},
				MaxConcurrentStreams: 10,
			},
			Monitoring: *newServerConfigWithDefaultTLS("sidecar", 2114),
		},
		expectedServiceConfig: &sidecar.Config{
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
			c, serverConfig, err := ReadSidecarYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedServiceConfig, c)
			require.Equal(t, tc.expectedServerConfig, serverConfig)
		})
	}
}

func TestReadConfigCoordinator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                  string
		configFilePath        string
		expectedServiceConfig *coordinator.Config
		expectedServerConfig  *serve.Config
	}{{
		name:                 "default",
		configFilePath:       emptyConfig(t),
		expectedServerConfig: newServeConfig(coordinator.DefaultServerPort, coordinator.DefaultMonitoringPort),
		expectedServiceConfig: &coordinator.Config{
			DependencyGraph: &coordinator.DependencyGraphConfig{
				NumOfLocalDepConstructors: coordinator.DefaultNumOfLocalDepConstructors,
				WaitingTxsLimit:           coordinator.DefaultWaitingTxsLimit,
			},
			ChannelBufferSizePerGoroutine: coordinator.DefaultChannelBufferSizePerGoroutine,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/coordinator.yaml",
		expectedServerConfig: newServeConfigWithDefaultTLS(
			"coordinator", coordinator.DefaultServerPort, coordinator.DefaultMonitoringPort,
		),
		expectedServiceConfig: &coordinator.Config{
			Verifier: newMultiClientConfigWithDefaultTLS(
				"verifier", "coordinator", verifier.DefaultServerPort,
			),
			ValidatorCommitter: newMultiClientConfigWithDefaultTLS("vc", "coordinator", vc.DefaultServerPort),
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
			c, serverConfig, err := ReadCoordinatorYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedServiceConfig, c)
			require.Equal(t, tc.expectedServerConfig, serverConfig)
		})
	}
}

func TestReadConfigVC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                  string
		configFilePath        string
		expectedServiceConfig *vc.Config
		expectedServerConfig  *serve.Config
	}{{
		name:                 "default",
		configFilePath:       emptyConfig(t),
		expectedServerConfig: newServeConfig(vc.DefaultServerPort, vc.DefaultMonitoringPort),
		expectedServiceConfig: &vc.Config{
			Database: defaultDBConfig(),
			ResourceLimits: &vc.ResourceLimitsConfig{
				MaxWorkersForPreparer:             vc.DefaultMaxWorkersForPreparer,
				MaxWorkersForValidator:            vc.DefaultMaxWorkersForValidator,
				MaxWorkersForCommitter:            vc.DefaultMaxWorkersForCommitter,
				MinTransactionBatchSize:           vc.DefaultMinTransactionBatchSize,
				TimeoutForMinTransactionBatchSize: vc.DefaultTimeoutForMinBatchSize,
			},
		},
	}, {
		name:                 "sample",
		configFilePath:       "samples/vc.yaml",
		expectedServerConfig: newServeConfigWithDefaultTLS("vc", vc.DefaultServerPort, vc.DefaultMonitoringPort),
		expectedServiceConfig: &vc.Config{
			Database: defaultSampleDBConfig(),
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
			c, serverConfig, err := ReadVCYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedServiceConfig, c)
			require.Equal(t, tc.expectedServerConfig, serverConfig)
		})
	}
}

func TestReadConfigVerifier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                  string
		configFilePath        string
		expectedServiceConfig *verifier.Config
		expectedServerConfig  *serve.Config
	}{{
		name:                 "default",
		configFilePath:       emptyConfig(t),
		expectedServerConfig: newServeConfig(verifier.DefaultServerPort, verifier.DefaultMonitoringPort),
		expectedServiceConfig: &verifier.Config{
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
		expectedServerConfig: newServeConfigWithDefaultTLS(
			"verifier", verifier.DefaultServerPort, verifier.DefaultMonitoringPort,
		),
		expectedServiceConfig: &verifier.Config{
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
			c, serverConfig, err := ReadVerifierYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedServiceConfig, c)
			require.Equal(t, tc.expectedServerConfig, serverConfig)
		})
	}
}

func TestReadConfigQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                  string
		configFilePath        string
		expectedServiceConfig *query.Config
		expectedServerConfig  *serve.Config
	}{{
		name:           "default",
		configFilePath: emptyConfig(t),
		expectedServerConfig: &serve.Config{
			GRPC: serve.ServerConfig{
				Endpoint: *newEndpoint(connection.DefaultHost, query.DefaultServerPort),
				RateLimit: serve.RateLimitConfig{
					RequestsPerSecond: query.DefaultRequestsPerSecond,
					Burst:             query.DefaultBurst,
				},
			},
			Monitoring: *newServerConfig(connection.DefaultHost, query.DefaultMonitoringPort),
		},
		expectedServiceConfig: &query.Config{
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
		expectedServerConfig: newServeConfigWithDefaultTLS(
			"query", query.DefaultServerPort, query.DefaultMonitoringPort,
		),
		expectedServiceConfig: &query.Config{
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
			c, serverConfig, err := ReadQueryYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedServiceConfig, c)
			require.Equal(t, tc.expectedServerConfig, serverConfig)
		})
	}
}

func TestReadConfigLoadGen(t *testing.T) {
	t.Parallel()
	loadgenTLSCreds := test.NewServiceTLSConfig(artifactsPath, "loadgen", connection.MutualTLSMode)
	tests := []struct {
		name                  string
		configFilePath        string
		expectedServiceConfig *loadgen.ClientConfig
		expectedServerConfig  *serve.Config
	}{{
		name:                  "default",
		configFilePath:        emptyConfig(t),
		expectedServerConfig:  newServeConfig(loadgen.DefaultServerPort, loadgen.DefaultMonitoringPort),
		expectedServiceConfig: &loadgen.ClientConfig{},
	}, {
		name:           "sample",
		configFilePath: "samples/loadgen.yaml",
		expectedServerConfig: &serve.Config{
			GRPC:       *newServerConfigWithDefaultTLS("loadgen", loadgen.DefaultServerPort),
			Monitoring: *newServerConfigWithDefaultTLS("loadgen", loadgen.DefaultMonitoringPort),
			HTTP:       *newServerConfig("", 6997),
		},
		expectedServiceConfig: &loadgen.ClientConfig{
			Monitoring: metrics.Config{
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
			c, serverConfig, err := ReadLoadGenYamlAndSetupLogging(v, tc.configFilePath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedServiceConfig, c)
			require.Equal(t, tc.expectedServerConfig, serverConfig)
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

func newServeConfigWithDefaultTLS(host string, grpcPort, monitorinPort int) *serve.Config {
	return &serve.Config{
		GRPC:       *newServerConfigWithDefaultTLS(host, grpcPort),
		Monitoring: *newServerConfigWithDefaultTLS(host, monitorinPort),
	}
}

func newServeConfig(grpcPort, monitorinPort int) *serve.Config {
	return &serve.Config{
		GRPC:       *newServerConfig(connection.DefaultHost, grpcPort),
		Monitoring: *newServerConfig(connection.DefaultHost, monitorinPort),
	}
}

func newServerConfigWithDefaultTLS(serviceName string, port int) *serve.ServerConfig {
	return &serve.ServerConfig{
		Endpoint: *newEndpoint("", port),
		TLS:      test.NewServiceTLSConfig(artifactsPath, serviceName, connection.MutualTLSMode),
	}
}

func newServerConfig(host string, port int) *serve.ServerConfig {
	return &serve.ServerConfig{
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
		MSPDir: "/root/artifacts/peerOrganizations/peer-org-0/users/client@peer-org-0.com/msp",
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
