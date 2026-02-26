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
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

var (
	defaultServerTLSConfig = connection.TLSConfig{
		Mode:     connection.MutualTLSMode,
		CertPath: "/server-certs/public-key.pem",
		KeyPath:  "/server-certs/private-key.pem",
		CACertPaths: []string{
			"/server-certs/ca-certificate.pem",
		},
	}
	defaultClientTLSConfig = connection.TLSConfig{
		Mode:     connection.MutualTLSMode,
		CertPath: "/client-certs/public-key.pem",
		KeyPath:  "/client-certs/private-key.pem",
		CACertPaths: []string{
			"/client-certs/ca-certificate.pem",
		},
	}
)

const ordererRootCA = "/client-certs/orderer-ca-certificate.pem"

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
			Server:     newServerConfig("localhost", 4001),
			Monitoring: newServerConfig("localhost", 2114),
			Orderer: ordererconn.Config{
				ChannelID: "mychannel",
			},
			Committer: &connection.ClientConfig{
				Endpoint: newEndpoint("localhost", 9001),
			},
			Ledger: sidecar.LedgerConfig{
				Path: "./ledger/",
			},
			Notification: sidecar.NotificationServiceConfig{
				MaxTimeout: time.Minute,
			},
			LastCommittedBlockSetInterval: 3 * time.Second,
			WaitingTxsLimit:               100_000,
			ChannelBufferSize:             100,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/sidecar.yaml",
		expectedConfig: &sidecar.Config{
			Server: &connection.ServerConfig{
				Endpoint: *newEndpoint("", 4001),
				TLS:      defaultServerTLSConfig,
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
			Monitoring: newServerConfigWithDefaultTLS(2114),
			Orderer: ordererconn.Config{
				ChannelID: "mychannel",
				TLS: ordererconn.OrdererTLSConfig{
					Mode:              defaultClientTLSConfig.Mode,
					KeyPath:           defaultClientTLSConfig.KeyPath,
					CertPath:          defaultClientTLSConfig.CertPath,
					CommonCACertPaths: defaultClientTLSConfig.CACertPaths,
				},
				Organizations: map[string]*ordererconn.OrganizationConfig{
					"org0": {
						Endpoints: []*commontypes.OrdererEndpoint{
							newOrdererEndpoint("", "orderer"),
						},
						CACerts: defaultClientTLSConfig.CACertPaths,
					},
				},
				Identity: newIdentityConfig(),
			},
			Committer: newClientConfigWithDefaultTLS("coordinator", 9001),
			Ledger: sidecar.LedgerConfig{
				Path:         "/root/sc/ledger",
				SyncInterval: 100,
			},
			Notification: sidecar.NotificationServiceConfig{
				MaxTimeout: 10 * time.Minute,
			},
			LastCommittedBlockSetInterval: 5 * time.Second,
			WaitingTxsLimit:               20_000_000,
			ChannelBufferSize:             100,
			Bootstrap: sidecar.Bootstrap{
				GenesisBlockFilePath: "/root/material/config-block.pb.bin",
			},
		},
	}}
	for _, tt := range tests {
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
			Server:     newServerConfig("localhost", 9001),
			Monitoring: newServerConfig("localhost", 2119),
			DependencyGraph: &coordinator.DependencyGraphConfig{
				NumOfLocalDepConstructors: 1,
				WaitingTxsLimit:           100_000,
			},
			ChannelBufferSizePerGoroutine: 10,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/coordinator.yaml",
		expectedConfig: &coordinator.Config{
			Server:             newServerConfigWithDefaultTLS(9001),
			Monitoring:         newServerConfigWithDefaultTLS(2119),
			Verifier:           newMultiClientConfigWithDefaultTLS("verifier", 5001),
			ValidatorCommitter: newMultiClientConfigWithDefaultTLS("vc", 6001),
			DependencyGraph: &coordinator.DependencyGraphConfig{
				NumOfLocalDepConstructors: 1,
				WaitingTxsLimit:           100_000,
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
			Server:     newServerConfig("localhost", 6001),
			Monitoring: newServerConfig("localhost", 2116),
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
		configFilePath: "samples/vc.yaml",
		expectedConfig: &vc.Config{
			Server:     newServerConfigWithDefaultTLS(6001),
			Monitoring: newServerConfigWithDefaultTLS(2116),
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
			Server:     newServerConfig("localhost", 5001),
			Monitoring: newServerConfig("localhost", 2115),
			ParallelExecutor: verifier.ExecutorConfig{
				Parallelism:       4,
				BatchSizeCutoff:   50,
				BatchTimeCutoff:   500 * time.Millisecond,
				ChannelBufferSize: 50,
			},
		},
	}, {
		name:           "sample",
		configFilePath: "samples/verifier.yaml",
		expectedConfig: &verifier.Config{
			Server:     newServerConfigWithDefaultTLS(5001),
			Monitoring: newServerConfigWithDefaultTLS(2115),
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
			Server: &connection.ServerConfig{
				Endpoint: *newEndpoint("localhost", 7001),
				RateLimit: connection.RateLimitConfig{
					RequestsPerSecond: 5000,
					Burst:             1000,
				},
			},
			Monitoring:            newServerConfig("localhost", 2117),
			Database:              defaultDBConfig(),
			MinBatchKeys:          1024,
			MaxBatchWait:          100 * time.Millisecond,
			ViewAggregationWindow: 100 * time.Millisecond,
			MaxAggregatedViews:    1024,
			MaxActiveViews:        4096,
			MaxViewTimeout:        10 * time.Second,
			MaxRequestKeys:        10000,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/query.yaml",
		expectedConfig: &query.Config{
			Server:                newServerConfigWithDefaultTLS(7001),
			Monitoring:            newServerConfigWithDefaultTLS(2117),
			Database:              defaultSampleDBConfig(),
			MinBatchKeys:          1024,
			MaxBatchWait:          100 * time.Millisecond,
			ViewAggregationWindow: 100 * time.Millisecond,
			MaxAggregatedViews:    1024,
			MaxActiveViews:        4096,
			MaxViewTimeout:        10 * time.Second,
			MaxRequestKeys:        10000,
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
			Server: newServerConfig("localhost", 8001),
			Monitoring: metrics.Config{
				ServerConfig: *newServerConfig("localhost", 2118),
			},
		},
	}, {
		name:           "sample",
		configFilePath: "samples/loadgen.yaml",
		expectedConfig: &loadgen.ClientConfig{
			Server:     newServerConfigWithDefaultTLS(8001),
			HTTPServer: newServerConfig("", 6997),
			Monitoring: metrics.Config{
				ServerConfig: *newServerConfigWithDefaultTLS(2118),
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
					SidecarClient: newClientConfigWithDefaultTLS("sidecar", 4001),
					Orderer: ordererconn.Config{
						ChannelID:     "mychannel",
						ConsensusType: ordererconn.Bft,
						Identity:      newIdentityConfig(),
						TLS: ordererconn.OrdererTLSConfig{
							Mode:              defaultClientTLSConfig.Mode,
							KeyPath:           defaultClientTLSConfig.KeyPath,
							CertPath:          defaultClientTLSConfig.CertPath,
							CommonCACertPaths: defaultClientTLSConfig.CACertPaths,
						},
						Organizations: map[string]*ordererconn.OrganizationConfig{
							"org0": {
								Endpoints: []*commontypes.OrdererEndpoint{
									newOrdererEndpoint("", "orderer"),
								},
								CACerts: []string{ordererRootCA},
							},
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
						workload.DefaultGeneratedNamespaceID: {
							Scheme: signature.Ecdsa, Seed: 10,
						},
					},
					OrdererEndpoints: []*commontypes.OrdererEndpoint{
						newOrdererEndpoint("org", "orderer"),
					},
					PeerOrganizationCount: 2,
					CryptoMaterialPath:    "/root/material",
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
		Endpoints:      []*connection.Endpoint{newEndpoint("localhost", 5433)},
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
		Endpoints: []*connection.Endpoint{newEndpoint("db", 5433)},
		Username:  "yugabyte",
		Password:  "yugabyte",
		Database:  "yugabyte",
		TLS: dbconn.DatabaseTLSConfig{
			Mode:       connection.OneSideTLSMode,
			CACertPath: "/server-certs/ca-certificate.pem",
		},
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

func newClientConfigWithDefaultTLS(host string, port int) *connection.ClientConfig {
	return &connection.ClientConfig{
		Endpoint: newEndpoint(host, port),
		TLS:      defaultClientTLSConfig,
	}
}

func newMultiClientConfigWithDefaultTLS(host string, port int) connection.MultiClientConfig {
	return connection.MultiClientConfig{
		Endpoints: []*connection.Endpoint{
			newEndpoint(host, port),
		},
		TLS: defaultClientTLSConfig,
	}
}

func newServerConfigWithDefaultTLS(port int) *connection.ServerConfig {
	return &connection.ServerConfig{
		Endpoint: *newEndpoint("", port),
		TLS:      defaultServerTLSConfig,
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

func newIdentityConfig() *ordererconn.IdentityConfig {
	return &ordererconn.IdentityConfig{
		MspID:  "peer-org-0",
		MSPDir: "/root/material/peerOrganizations/peer-org-0/users/client@peer-org-0.com/msp",
		BCCSP: &factory.FactoryOpts{
			Default: "SW",
			SW: &factory.SwOpts{
				Hash:     "SHA2",
				Security: 256,
			},
		},
	}
}

func newOrdererEndpoint(mspID, host string) *commontypes.OrdererEndpoint {
	return &commontypes.OrdererEndpoint{
		ID:    0,
		MspID: mspID,
		Host:  host,
		Port:  7050,
		API:   []string{commontypes.Broadcast, commontypes.Deliver},
	}
}

func emptyConfig(t *testing.T) string {
	t.Helper()
	configPath := filepath.Clean(path.Join(t.TempDir(), "empty.yaml"))
	require.NoError(t, os.WriteFile(configPath, []byte{}, 0o660))
	return configPath
}
