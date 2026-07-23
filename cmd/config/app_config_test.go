/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
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
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	artifactsPath       = "/root/artifacts"
	defaultDatabaseName = "yugabyte"
)

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
				Endpoint:             *newEndpoint(connection.DefaultHost, sidecarServerPort),
				RateLimit:            serve.RateLimitConfig{RequestsPerSecond: 5000, Burst: 1000},
				MaxConcurrentStreams: 10,
			},
			HTTP:                  *newServerConfig(sidecarMonitoringPort),
			ServiceStartupTimeout: serve.DefaultServiceStartupTimeout,
		},
		expectedServiceConfig: &sidecar.Config{
			Committer: &connection.ClientConfig{
				Endpoint: newEndpoint(connection.DefaultHost, coordinatorServerPort),
			},
			Orderer: ordererdial.Config{
				SuspicionGracePeriodPerBlock: time.Second,
			},
			Ledger: sidecar.LedgerConfig{
				Path: "./ledger/",
			},
			Notification: sidecar.NotificationServiceConfig{
				MaxTimeout:         time.Minute,
				MaxActiveTxIDs:     100_000,
				MaxTxIDsPerRequest: 1000,
				StreamWriteTimeout: 30 * time.Second,
			},
			LastCommittedBlockSetInterval: 5 * time.Second,
			WaitingTxsLimit:               100_000,
			ChannelBufferSize:             100,
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
			HTTP:                  *newServerConfigWithDefaultTLS("sidecar", 2114),
			ServiceStartupTimeout: serve.DefaultServiceStartupTimeout,
		},
		expectedServiceConfig: &sidecar.Config{
			Orderer: ordererdial.Config{
				FaultToleranceLevel:        ordererdial.BFT,
				LatestKnownConfigBlockPath: "/root/artifacts/config-block.pb.bin",
				Identity:                   newIdentityConfig(),
				TLS: connection.TLSConfig{
					Mode:        sidecarTLSCreds.Mode,
					KeyPath:     sidecarTLSCreds.KeyPath,
					CertPath:    sidecarTLSCreds.CertPath,
					CACertPaths: sidecarTLSCreds.CACertPaths,
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
				StreamWriteTimeout: 30 * time.Second,
			},
			LastCommittedBlockSetInterval: 5 * time.Second,
			WaitingTxsLimit:               20_000_000,
			ChannelBufferSize:             100,
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
		expectedServerConfig: newServeConfig(coordinatorServerPort, coordinatorMonitoringPort),
		expectedServiceConfig: &coordinator.Config{
			Verifier: connection.MultiClientConfig{
				Endpoints: []*connection.Endpoint{newEndpoint(connection.DefaultHost, verifierServerPort)},
			},
			ValidatorCommitter: connection.MultiClientConfig{
				Endpoints: []*connection.Endpoint{newEndpoint(connection.DefaultHost, vcServerPort)},
			},
			DependencyGraph: &coordinator.DependencyGraphConfig{
				NumOfLocalDepConstructors: 1,
				WaitingTxsLimit:           100_000,
				ChunkSize:                 500,
			},
			ChannelBufferSizePerGoroutine: 10,
			QueueMonitorSamplingTime:      100 * time.Millisecond,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/coordinator.yaml",
		expectedServerConfig: newServeConfigWithDefaultTLS(
			"coordinator", coordinatorServerPort, coordinatorMonitoringPort,
		),
		expectedServiceConfig: &coordinator.Config{
			Verifier: newMultiClientConfigWithDefaultTLS(
				"verifier", "coordinator", verifierServerPort,
			),
			ValidatorCommitter: newMultiClientConfigWithDefaultTLS("vc", "coordinator", vcServerPort),
			DependencyGraph: &coordinator.DependencyGraphConfig{
				NumOfLocalDepConstructors: 1,
				WaitingTxsLimit:           100_000,
				ChunkSize:                 500,
			},
			ChannelBufferSizePerGoroutine: 10,
			QueueMonitorSamplingTime:      100 * time.Millisecond,
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
		expectedServerConfig: newServeConfig(vcServerPort, vcMonitoringPort),
		expectedServiceConfig: &vc.Config{
			Database: defaultDBConfig(),
			ResourceLimits: &vc.ResourceLimitsConfig{
				MaxWorkersForPreparer:             1,
				MaxWorkersForValidator:            1,
				MaxWorkersForCommitter:            20,
				MinTransactionBatchSize:           1,
				TimeoutForMinTransactionBatchSize: 5 * time.Second,
			},
		},
	}, {
		name:                 "sample",
		configFilePath:       "samples/vc.yaml",
		expectedServerConfig: newServeConfigWithDefaultTLS("vc", vcServerPort, vcMonitoringPort),
		expectedServiceConfig: &vc.Config{
			Database: defaultSampleDBConfig(),
			ResourceLimits: &vc.ResourceLimitsConfig{
				MaxWorkersForPreparer:             1,
				MaxWorkersForValidator:            1,
				MaxWorkersForCommitter:            20,
				MinTransactionBatchSize:           1,
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
		expectedServerConfig: newServeConfig(verifierServerPort, verifierMonitoringPort),
		expectedServiceConfig: &verifier.Config{
			Parallelism:       4,
			BatchSizeCutoff:   50,
			BatchTimeCutoff:   500 * time.Millisecond,
			ChannelBufferSize: 50,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/verifier.yaml",
		expectedServerConfig: newServeConfigWithDefaultTLS(
			"verifier", verifierServerPort, verifierMonitoringPort,
		),
		expectedServiceConfig: &verifier.Config{
			BatchSizeCutoff:   50,
			BatchTimeCutoff:   10 * time.Millisecond,
			ChannelBufferSize: 50,
			Parallelism:       40,
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
				Endpoint: *newEndpoint(connection.DefaultHost, queryServerPort),
				RateLimit: serve.RateLimitConfig{
					RequestsPerSecond: 5000,
					Burst:             1000,
				},
				MaxConcurrentStreams: 10,
			},
			HTTP:                  *newServerConfig(queryMonitoringPort),
			ServiceStartupTimeout: serve.DefaultServiceStartupTimeout,
		},
		expectedServiceConfig: &query.Config{
			Database:              defaultDBConfig(),
			MinBatchKeys:          1024,
			MaxBatchWait:          100 * time.Millisecond,
			ViewAggregationWindow: 100 * time.Millisecond,
			MaxAggregatedViews:    1024,
			MaxActiveViews:        4096,
			MaxViewTimeout:        10 * time.Second,
			MaxRequestKeys:        10000,
			TLSRefreshInterval:    time.Minute,
		},
	}, {
		name:           "sample",
		configFilePath: "samples/query.yaml",
		expectedServerConfig: withClientStreamLimit(newServeConfigWithDefaultTLS(
			"query", queryServerPort, queryMonitoringPort,
		)),
		expectedServiceConfig: &query.Config{
			Database:              defaultSampleDBConfig(),
			MinBatchKeys:          1024,
			MaxBatchWait:          100 * time.Millisecond,
			ViewAggregationWindow: 100 * time.Millisecond,
			MaxAggregatedViews:    1024,
			MaxActiveViews:        4096,
			MaxViewTimeout:        10 * time.Second,
			MaxRequestKeys:        10000,
			TLSRefreshInterval:    time.Minute,
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
		expectedServerConfig:  newServeConfig(loadgenServerPort, loadgenMonitoringPort),
		expectedServiceConfig: &loadgen.ClientConfig{},
	}, {
		name:           "sample",
		configFilePath: "samples/loadgen.yaml",
		expectedServerConfig: newServeConfigWithDefaultTLS(
			"loadgen", loadgenServerPort, loadgenMonitoringPort,
		),
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
						TLS: connection.TLSConfig{
							Mode:        loadgenTLSCreds.Mode,
							KeyPath:     loadgenTLSCreds.KeyPath,
							CertPath:    loadgenTLSCreds.CertPath,
							CACertPaths: loadgenTLSCreds.CACertPaths,
						},
					},
					BroadcastParallelism: 1,
				},
			},
			LoadProfile: &workload.Profile{
				Block: workload.BlockProfile{
					MaxSize:       500,
					MinSize:       10,
					PreferredRate: time.Second,
				},
				Transaction: workload.TransactionProfile{
					KeySize:           32,
					ReadWriteCount:    2,
					InvalidSignatures: 0.1,
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

// TestLoadGenProbabilityValidation exercises the decode-and-validate path for the loadgen transaction
// conflict probabilities: values inside the closed interval [0,1] are accepted, values outside are
// rejected by validation. This also covers the per-dependency probability, which lives inside a slice
// (validated via "dive").
func TestLoadGenProbabilityValidation(t *testing.T) {
	t.Parallel()
	read := func(t *testing.T, yaml string) error {
		t.Helper()
		v := viper.New()
		require.NoError(t, readYamlConfigsFromIO(v, strings.NewReader(yaml)))
		return unmarshal(v, &loadgen.ClientConfig{})
	}
	invalidSig := func(probability string) string {
		return "load-profile:\n  transaction:\n    invalid-signatures: " + probability + "\n"
	}
	dependency := func(probability string) string {
		return "load-profile:\n  transaction:\n    dependencies:\n" +
			"      - probability: " + probability + "\n        src: read\n        dst: write\n"
	}

	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{name: "invalid-signatures in range", yaml: invalidSig("0.3")},
		{name: "invalid-signatures is 1", yaml: invalidSig("1")},
		{name: "invalid-signatures above 1", yaml: invalidSig("1.5"), wantErr: true},
		{name: "invalid-signatures below 0", yaml: invalidSig("-0.1"), wantErr: true},
		{name: "dependency probability in range", yaml: dependency("0.5")},
		{name: "dependency probability above 1", yaml: dependency("2"), wantErr: true},
		{name: "dependency probability below 0", yaml: dependency("-1"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := read(t, tc.yaml)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func defaultDBConfig() *statedb.Config {
	return &statedb.Config{
		Endpoints:      []*connection.Endpoint{newEndpoint(connection.DefaultHost, 5433)},
		Database:       defaultDatabaseName,
		MaxConnections: 20,
		MinConnections: 1,
		Retry: &retry.Profile{
			MaxElapsedTime: new(10 * time.Minute),
		},
	}
}

func defaultSampleDBConfig() *statedb.Config {
	return &statedb.Config{
		Endpoints: []*connection.Endpoint{newEndpoint("db", 5433)},
		Username:  "yugabyte",
		Password:  "yugabyte",
		Database:  defaultDatabaseName,
		TLS: statedb.TLSConfig{
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
			MaxElapsedTime:      new(15 * time.Minute),
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
		GRPC:                  *newServerConfigWithDefaultTLS(host, grpcPort),
		HTTP:                  *newServerConfigWithDefaultTLS(host, monitorinPort),
		ServiceStartupTimeout: serve.DefaultServiceStartupTimeout,
	}
}

func newServeConfig(grpcPort, monitorinPort int) *serve.Config {
	return &serve.Config{
		GRPC:                  *newServerConfig(grpcPort),
		HTTP:                  *newServerConfig(monitorinPort),
		ServiceStartupTimeout: serve.DefaultServiceStartupTimeout,
	}
}

// withClientStreamLimit sets the client-facing gRPC max-concurrent-streams default (10) on c's
// server and returns it. Only the client-facing services (query, sidecar) get this default.
func withClientStreamLimit(c *serve.Config) *serve.Config {
	c.GRPC.MaxConcurrentStreams = 10
	return c
}

func newServerConfigWithDefaultTLS(serviceName string, port int) *serve.ServerConfig {
	return &serve.ServerConfig{
		Endpoint: *newEndpoint("", port),
		TLS:      test.NewServiceTLSConfig(artifactsPath, serviceName, connection.MutualTLSMode),
	}
}

func newServerConfig(port int) *serve.ServerConfig {
	return &serve.ServerConfig{
		Endpoint: *newEndpoint(connection.DefaultHost, port),
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

// TestReconnectMaxElapsedTime exercises the full decode-and-validate path for the
// reconnect retry profile: an explicit 0 requests unlimited retries and must decode
// to a non-nil pointer, an omitted value must stay nil (so the 15m default applies
// later), and a negative value must be rejected by validation.
func TestReconnectMaxElapsedTime(t *testing.T) {
	t.Parallel()
	read := func(t *testing.T, yaml string) (*connection.MultiClientConfig, error) {
		t.Helper()
		v := viper.New()
		require.NoError(t, readYamlConfigsFromIO(v, strings.NewReader(yaml)))
		mc := &connection.MultiClientConfig{}
		return mc, unmarshal(v, mc)
	}

	t.Run("zero decodes to unlimited", func(t *testing.T) {
		t.Parallel()
		mc, err := read(t, "reconnect:\n  max-elapsed-time: 0s\n")
		require.NoError(t, err)
		require.NotNil(t, mc.Retry)
		require.NotNil(t, mc.Retry.MaxElapsedTime)
		require.Equal(t, time.Duration(0), *mc.Retry.MaxElapsedTime)
	})

	t.Run("omitted stays nil", func(t *testing.T) {
		t.Parallel()
		mc, err := read(t, "reconnect:\n  initial-interval: 1s\n")
		require.NoError(t, err)
		require.NotNil(t, mc.Retry)
		require.Nil(t, mc.Retry.MaxElapsedTime)
	})

	t.Run("negative is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := read(t, "reconnect:\n  max-elapsed-time: -5m\n")
		require.Error(t, err)
	})
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

// TestLoggingDefaults verifies the logging defaults: logSpec comes from the struct-assignment tag
// on loggingConfig, and format defaults to its empty zero value.
func TestLoggingDefaults(t *testing.T) {
	t.Parallel()
	v := NewViperWithLoggingDefault("test")
	c := new(loggingConfig)
	require.NoError(t, unmarshal(v, c))
	require.Equal(t, "info:grpc=error", c.Logging.LogSpec)
	require.Empty(t, c.Logging.Format)
}

// TestEnvOverrideFieldsNotInYAML verifies that environment variables can override
// config fields even when those fields are not present in the YAML file and have no defaults.
func TestEnvOverrideFieldsNotInYAML(t *testing.T) {
	f := emptyConfig(t)

	for _, tc := range []struct{ name, file string }{
		{name: "coordinator-empty", file: f},
		{name: "coordinator-sample", file: "samples/coordinator.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SC_COORDINATOR_VALIDATOR_COMMITTER_RECONNECT_MULTIPLIER", "1.1")
			t.Setenv("SC_COORDINATOR_SERVER_TLS_CERT_PATH", "/path/server")
			t.Setenv("SC_COORDINATOR_MONITORING_TLS_CERT_PATH", "/path/monitoring")
			t.Setenv("SC_COORDINATOR_VERIFIER_TLS_CERT_PATH", "/path/verifier")
			t.Setenv("SC_COORDINATOR_VALIDATOR_COMMITTER_TLS_CERT_PATH", "/path/vc")
			conf, server, err := ReadCoordinatorYamlAndSetupLogging(NewViperWithCoordinatorDefaults(), tc.file)
			require.NoError(t, err)
			require.NotNil(t, conf.ValidatorCommitter.Retry)
			assert.InEpsilon(t, 1.1, conf.ValidatorCommitter.Retry.Multiplier, 1e-4)
			assert.Equal(t, "/path/server", server.GRPC.TLS.CertPath)
			assert.Equal(t, "/path/monitoring", server.HTTP.TLS.CertPath)
			assert.Equal(t, "/path/verifier", conf.Verifier.TLS.CertPath)
			assert.Equal(t, "/path/vc", conf.ValidatorCommitter.TLS.CertPath)
		})
	}

	for _, tc := range []struct{ name, file string }{
		{name: "sidecar-empty", file: f},
		{name: "sidecar-sample", file: "samples/sidecar.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SC_SIDECAR_LEDGER_SYNC_INTERVAL", "60")
			t.Setenv("SC_SIDECAR_ORDERER_RECONNECT_MAX_INTERVAL", "1m")
			t.Setenv("SC_SIDECAR_SERVER_TLS_MODE", "none")
			t.Setenv("SC_SIDECAR_ORDERER_TLS_MODE", "mtls")
			t.Setenv("SC_SIDECAR_COMMITTER_TLS_MODE", "tls")
			c, server, err := ReadSidecarYamlAndSetupLogging(NewViperWithSidecarDefaults(), tc.file)
			require.NoError(t, err)
			assert.Equal(t, uint64(60), c.Ledger.SyncInterval)
			require.NotNil(t, c.Orderer.Retry)
			require.Equal(t, time.Minute, c.Orderer.Retry.MaxInterval)
			require.Equal(t, "none", server.GRPC.TLS.Mode)
			require.Equal(t, "mtls", c.Orderer.TLS.Mode)
			require.Equal(t, "tls", c.Committer.TLS.Mode)
		})
	}

	for _, tc := range []struct{ name, file string }{
		{name: "vc-empty", file: f},
		{name: "vc-sample", file: "samples/vc.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SC_VC_DATABASE_TABLE_PRE_SPLIT_TABLETS", "10")
			t.Setenv("SC_VC_DATABASE_LOAD_BALANCE", "true")
			t.Setenv("SC_VC_SERVER_TLS_KEY_PATH", "/path/server")
			t.Setenv("SC_VC_MONITORING_TLS_KEY_PATH", "/path/monitoring")
			conf, server, err := ReadVCYamlAndSetupLogging(NewViperWithVCDefaults(), tc.file)
			require.NoError(t, err)
			assert.True(t, conf.Database.LoadBalance)
			assert.Equal(t, 10, conf.Database.TablePreSplitTablets)
			assert.Equal(t, "/path/server", server.GRPC.TLS.KeyPath)
			assert.Equal(t, "/path/monitoring", server.HTTP.TLS.KeyPath)
		})
	}

	for _, tc := range []struct{ name, file string }{
		{name: "query-empty", file: f},
		{name: "query-sample", file: "samples/query.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SC_QUERY_MAX_ACTIVE_VIEWS", "8192")
			t.Setenv("SC_QUERY_DATABASE_TABLE_PRE_SPLIT_TABLETS", "512")
			t.Setenv("SC_QUERY_SERVER_TLS_CA_CERT_PATHS", "/path/server")
			t.Setenv("SC_QUERY_MONITORING_TLS_CA_CERT_PATHS", "[/path/mon1,/path/mon2]")
			conf, server, err := ReadQueryYamlAndSetupLogging(NewViperWithQueryDefaults(), tc.file)
			require.NoError(t, err)
			assert.Equal(t, 8192, conf.MaxActiveViews)
			assert.Equal(t, 512, conf.Database.TablePreSplitTablets)
			assert.Equal(t, []string{"/path/server"}, server.GRPC.TLS.CACertPaths)
			assert.Equal(t, []string{"/path/mon1", "/path/mon2"}, server.HTTP.TLS.CACertPaths)
		})
	}

	for _, tc := range []struct{ name, file string }{
		{name: "loadgen-empty", file: f},
		{name: "loadgen-sample", file: "samples/loadgen.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SC_LOADGEN_LOAD_PROFILE_TRANSACTION_WRITE_COUNT", "15")
			t.Setenv("SC_LOADGEN_YAML", `
load-profile:
  policy:
    namespace-policies:
      new:
        scheme: MSP
`)
			t.Setenv("SC_LOADGEN_COORDINATOR_CLIENT_RECONNECT_MULTIPLIER", "1.2")
			t.Setenv("SC_LOADGEN_SERVER_KEEP_ALIVE_PARAMS_MAX_CONNECTION_IDLE", "3m")
			t.Setenv("SC_LOADGEN_MONITORING_KEEP_ALIVE_PARAMS_MAX_CONNECTION_IDLE", "5m")
			conf, server, err := ReadLoadGenYamlAndSetupLogging(NewViperWithLoadGenDefaults(), tc.file)
			require.NoError(t, err)
			require.NotNil(t, conf.LoadProfile)
			assert.Equal(t, uint32(15), conf.LoadProfile.Transaction.BlindWriteCount)
			require.NotNil(t, conf.LoadProfile.Policy)
			require.NotNil(t, conf.LoadProfile.Policy.NamespacePolicies)
			require.NotNil(t, conf.LoadProfile.Policy.NamespacePolicies["new"])
			require.Equal(t, "MSP", conf.LoadProfile.Policy.NamespacePolicies["new"].Scheme)
			require.NotNil(t, conf.Adapter.CoordinatorClient)
			require.NotNil(t, conf.Adapter.CoordinatorClient.Retry)
			assert.InEpsilon(t, 1.2, conf.Adapter.CoordinatorClient.Retry.Multiplier, 1e-4)
			assert.Equal(t, 3*time.Minute, server.GRPC.KeepAlive.Params.MaxConnectionIdle)
			assert.Equal(t, 5*time.Minute, server.HTTP.KeepAlive.Params.MaxConnectionIdle)
		})
	}

	for _, tc := range []struct{ name, file string }{
		{name: "orderer-empty", file: f},
		{name: "orderer-sample", file: "samples/mock-orderer.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SC_ORDERER_SERVERS_ENDPOINT", "orderer:1234")
			t.Setenv("SC_ORDERER_SERVER_KEEP_ALIVE_PARAMS_TIMEOUT", "3m")
			t.Setenv("SC_ORDERER_MONITORING_KEEP_ALIVE_PARAMS_TIMEOUT", "5m")
			conf, server, err := ReadMockOrdererYamlAndSetupLogging(NewViperWithOrdererDefaults(), tc.file)
			require.NoError(t, err)
			require.Len(t, conf.Servers, 1)
			require.NotNil(t, conf.Servers[0])
			require.Equal(t, "orderer", conf.Servers[0].Endpoint.Host)
			require.Equal(t, 1234, conf.Servers[0].Endpoint.Port)
			assert.Equal(t, 3*time.Minute, server.GRPC.KeepAlive.Params.Timeout)
			assert.Equal(t, 5*time.Minute, server.HTTP.KeepAlive.Params.Timeout)
		})
	}
}
