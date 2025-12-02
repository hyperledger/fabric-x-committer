/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

var (
	// InsecureTLSConfig defines an empty tls config.
	InsecureTLSConfig connection.TLSConfig
	// defaultGrpcRetryProfile defines the retry policy for a gRPC client connection.
	defaultGrpcRetryProfile connection.RetryProfile
)

type (
	// GrpcServers holds the server instances and their respective configurations.
	GrpcServers struct {
		Servers []*grpc.Server
		Configs []*connection.ServerConfig
	}
)

// FailHandler registers a [gomega] fail handler.
func FailHandler(t *testing.T) {
	t.Helper()
	gomega.RegisterFailHandler(func(message string, _ ...int) {
		t.Helper()
		t.Errorf("received error message: %s", message)
		t.FailNow()
	})
}

// ServerToMultiClientConfig is used to create a multi client configuration from existing server(s).
func ServerToMultiClientConfig(servers ...*connection.ServerConfig) *connection.MultiClientConfig {
	endpoints := make([]*connection.Endpoint, len(servers))
	for i, server := range servers {
		endpoints[i] = &server.Endpoint
	}
	return &connection.MultiClientConfig{
		Endpoints: endpoints,
	}
}

// RunGrpcServerForTest starts a GRPC server using a register method.
// It handles the cleanup of the GRPC server at the end of a test, and ensure the test is ended
// only when the GRPC server is down.
// It also updates the server config endpoint port to the actual port if the configuration
// did not specify a port.
// The method asserts that the GRPC server did not end with failure.
func RunGrpcServerForTest(
	ctx context.Context, tb testing.TB, serverConfig *connection.ServerConfig, register func(server *grpc.Server),
) *grpc.Server {
	tb.Helper()
	listener, err := serverConfig.Listener(ctx)
	require.NoError(tb, err)
	server, err := serverConfig.GrpcServer()
	require.NoError(tb, err)

	if register != nil {
		register(server)
	} else {
		healthgrpc.RegisterHealthServer(server, connection.DefaultHealthCheckService())
	}

	var wg sync.WaitGroup
	tb.Cleanup(wg.Wait)
	tb.Cleanup(server.Stop)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// We use assert to prevent panicking for cleanup errors.
		assert.NoError(tb, server.Serve(listener))
	}()

	_ = context.AfterFunc(ctx, func() {
		server.Stop()
	})
	return server
}

// StartGrpcServersForTest starts multiple GRPC servers with a default configuration.
func StartGrpcServersForTest(
	ctx context.Context,
	t *testing.T,
	numService int,
	register func(*grpc.Server, int),
) *GrpcServers {
	t.Helper()
	sc := make([]*connection.ServerConfig, numService)
	for i := range sc {
		sc[i] = connection.NewLocalHostServerWithTLS(InsecureTLSConfig)
	}
	return StartGrpcServersWithConfigForTest(ctx, t, sc, register)
}

// StartGrpcServersWithConfigForTest starts multiple GRPC servers with given configurations.
func StartGrpcServersWithConfigForTest(
	ctx context.Context, t *testing.T, sc []*connection.ServerConfig, register func(*grpc.Server, int),
) *GrpcServers {
	t.Helper()
	grpcServers := make([]*grpc.Server, len(sc))
	for i, s := range sc {
		i := i
		grpcServers[i] = RunGrpcServerForTest(ctx, t, s, func(server *grpc.Server) {
			if register != nil {
				register(server, i)
			} else {
				healthgrpc.RegisterHealthServer(server, connection.DefaultHealthCheckService())
			}
		})
	}
	return &GrpcServers{
		Servers: grpcServers,
		Configs: sc,
	}
}

// RunServiceForTest runs a service using the given service method, and waits for it to be ready
// given the waitFunc method.
// It handles the cleanup of the service at the end of a test, and ensure the test is ended
// only when the service return.
// The method asserts that the service did not end with failure.
// Returns a ready flag that indicate that the service is done.
func RunServiceForTest(
	ctx context.Context,
	tb testing.TB,
	service func(ctx context.Context) error,
	waitFunc func(ctx context.Context) bool,
) *channel.Ready {
	tb.Helper()
	doneFlag := channel.NewReady()
	var wg sync.WaitGroup
	// NOTE: we should cancel the context before waiting for the completion. Therefore, the
	//       order of cleanup matters, which is last added first called.
	tb.Cleanup(wg.Wait)
	dCtx, cancel := context.WithCancel(ctx)
	tb.Cleanup(cancel)
	wg.Add(1)

	// We extract caller information to ensure we have sufficient information for debugging.
	pc, file, no, ok := runtime.Caller(1)
	require.True(tb, ok)
	go func() {
		defer wg.Done()
		defer doneFlag.SignalReady()
		// We use assert to prevent panicking for cleanup errors.
		assert.NoErrorf(tb, service(dCtx), "called from %s:%d\n\t%s", file, no, runtime.FuncForPC(pc).Name())
	}()

	if waitFunc == nil {
		return doneFlag
	}

	initCtx, initCancel := context.WithTimeout(dCtx, 2*time.Minute)
	tb.Cleanup(initCancel)
	require.True(tb, waitFunc(initCtx), "service is not ready")
	return doneFlag
}

// RunServiceAndGrpcForTest combines running a service and its GRPC server.
// It is intended for services that implements the Service API (i.e., command line services).
func RunServiceAndGrpcForTest(
	ctx context.Context,
	t *testing.T,
	service connection.Service,
	serverConfig ...*connection.ServerConfig,
) *channel.Ready {
	t.Helper()
	doneFlag := RunServiceForTest(ctx, t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(service.Run(ctx))
	}, service.WaitForReady)
	for _, server := range serverConfig {
		RunGrpcServerForTest(ctx, t, server, service.RegisterService)
	}
	return doneFlag
}

// WaitUntilGrpcServerIsReady uses the health check API to check a service readiness.
func WaitUntilGrpcServerIsReady(
	ctx context.Context,
	t *testing.T,
	conn grpc.ClientConnInterface,
) {
	t.Helper()
	if conn == nil {
		return
	}
	healthClient := healthgrpc.NewHealthClient(conn)
	res, err := healthClient.Check(ctx, nil, grpc.WaitForReady(true))
	assert.NotEqual(t, codes.Canceled, status.Code(err))
	require.NoError(t, err)
	require.Equal(t, healthgrpc.HealthCheckResponse_SERVING, res.Status)
}

// StatusRetriever provides implementation retrieve status of given transaction identifiers.
type StatusRetriever interface {
	GetTransactionsStatus(context.Context, *protoblocktx.QueryStatus, ...grpc.CallOption) (
		*protoblocktx.TransactionsStatus, error,
	)
}

// EnsurePersistedTxStatus fails the test if the given TX IDs does not match the expected status.
//
//nolint:revive // maximum number of arguments per function exceeded; max 4 but got 5.
func EnsurePersistedTxStatus(
	ctx context.Context,
	t *testing.T,
	r StatusRetriever,
	txIDs []string,
	expected map[string]*protoblocktx.StatusWithHeight,
) {
	t.Helper()
	if len(txIDs) == 0 {
		return
	}
	actualStatus, err := r.GetTransactionsStatus(ctx, &protoblocktx.QueryStatus{TxIDs: txIDs})
	require.NoError(t, err)
	require.EqualExportedValues(t, expected, actualStatus.Status)
}

// CheckServerStopped returns true if the grpc server listening on a
// given address has been stopped.
func CheckServerStopped(t *testing.T, addr string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	conn, err := grpc.DialContext( //nolint:staticcheck
		ctx,
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(), //nolint:staticcheck
	)
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

// SetupDebugging can be added for development to tests that required additional debugging info.
func SetupDebugging() {
	logging.SetupWithConfig(&logging.Config{
		Enabled:     true,
		Level:       logging.Debug,
		Caller:      true,
		Development: true,
	})
}

// NewSecuredConnection creates the default connection with given transport credentials.
func NewSecuredConnection(
	t *testing.T,
	endpoint connection.WithAddress,
	tlsConfig connection.TLSConfig,
) *grpc.ClientConn {
	t.Helper()
	return NewSecuredConnectionWithRetry(t, endpoint, tlsConfig, defaultGrpcRetryProfile)
}

// NewSecuredConnectionWithRetry creates the default connection with given transport credentials.
func NewSecuredConnectionWithRetry(
	t *testing.T,
	endpoint connection.WithAddress,
	tlsConfig connection.TLSConfig,
	retry connection.RetryProfile,
) *grpc.ClientConn {
	t.Helper()
	clientCreds, err := tlsConfig.ClientCredentials()
	require.NoError(t, err)
	conn, err := connection.NewConnection(connection.Parameters{
		Address: endpoint.Address(),
		Creds:   clientCreds,
		Retry:   &retry,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

// NewInsecureConnection creates the default connection with insecure credentials.
func NewInsecureConnection(t *testing.T, endpoint connection.WithAddress) *grpc.ClientConn {
	t.Helper()
	return NewInsecureConnectionWithRetry(t, endpoint, defaultGrpcRetryProfile)
}

// NewInsecureConnectionWithRetry creates the default dial config with insecure credentials.
func NewInsecureConnectionWithRetry(
	t *testing.T, endpoint connection.WithAddress, retry connection.RetryProfile,
) *grpc.ClientConn {
	t.Helper()
	conn, err := connection.NewConnection(connection.Parameters{
		Address: endpoint.Address(),
		Creds:   insecure.NewCredentials(),
		Retry:   &retry,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

// NewInsecureLoadBalancedConnection creates the default connection with insecure credentials.
func NewInsecureLoadBalancedConnection(t *testing.T, endpoints []*connection.Endpoint) *grpc.ClientConn {
	t.Helper()
	conn, err := connection.NewLoadBalancedConnection(&connection.MultiClientConfig{
		Endpoints: endpoints,
		Retry:     &defaultGrpcRetryProfile,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

// NewTLSMultiClientConfig creates a multi client configuration for test purposes
// given number of endpoints and a TLS configuration.
func NewTLSMultiClientConfig(
	tlsConfig connection.TLSConfig,
	ep ...*connection.Endpoint,
) *connection.MultiClientConfig {
	return &connection.MultiClientConfig{
		Endpoints: ep,
		TLS:       tlsConfig,
		Retry:     &defaultGrpcRetryProfile,
	}
}

// NewInsecureClientConfig creates a client configuration for test purposes given an endpoint.
func NewInsecureClientConfig(ep *connection.Endpoint) *connection.ClientConfig {
	return NewTLSClientConfig(InsecureTLSConfig, ep)
}

// NewTLSClientConfig creates a client configuration for test purposes given a single endpoint and creds.
func NewTLSClientConfig(tlsConfig connection.TLSConfig, ep *connection.Endpoint) *connection.ClientConfig {
	return &connection.ClientConfig{
		Endpoint: ep,
		TLS:      tlsConfig,
		Retry:    &defaultGrpcRetryProfile,
	}
}

// LogStruct logs a struct in a flat representation.
func LogStruct(t *testing.T, name string, v any) {
	t.Helper()
	// Marshal struct to JSON
	data, err := json.Marshal(v)
	require.NoError(t, err)

	// Unmarshal to map
	var m map[string]any
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	// Flatten
	flat := make(map[string]any)
	flatten("", m, flat)
	sb := &strings.Builder{}
	for _, k := range slices.Sorted(maps.Keys(flat)) {
		sb.WriteString(k)
		sb.WriteString(": ")
		_, printErr := fmt.Fprintf(sb, "%v", flat[k])
		require.NoError(t, printErr)
		sb.WriteString("\n")
	}
	t.Logf("%s:\n%s", name, sb.String())
}

func flatten(prefix string, in any, out map[string]any) {
	switch val := in.(type) {
	default:
		out[prefix] = val
	case map[string]any:
		e := flattenEndpoint(val)
		if e != nil {
			out[prefix] = e
			return
		}
		for k, v := range val {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, v, out)
		}
	case []any:
		for i, item := range val {
			key := fmt.Sprintf("%s.%d", prefix, i)
			flatten(key, item, out)
		}
	}
}

func flattenEndpoint(in map[string]any) *connection.Endpoint {
	if len(in) != 2 {
		return nil
	}
	host, okHost := in["host"]
	if !okHost {
		return nil
	}
	hostStr, okHostStr := host.(string)
	if !okHostStr {
		return nil
	}
	port, okPort := in["port"]
	if !okPort {
		return nil
	}
	portFloat, okPortFloat := port.(float64)
	if !okPortFloat {
		return nil
	}
	return &connection.Endpoint{Host: hostStr, Port: int(portFloat)}
}

// MustCreateEndpoint parses an endpoint from an address string.
// It panics if it fails to parse.
func MustCreateEndpoint(value string) *connection.Endpoint {
	endpoint, err := connection.NewEndpoint(value)
	if err != nil {
		panic(errors.Wrap(err, "could not create endpoint"))
	}
	return endpoint
}

// CreateEndorsementsForThresholdRule creates a slice of EndorsementSet pointers from individual threshold signatures.
// Each signature provided is wrapped in its own EndorsementWithIdentity and then placed
// in its own new EndorsementSet.
func CreateEndorsementsForThresholdRule(signatures ...[]byte) []*protoblocktx.Endorsements {
	sets := make([]*protoblocktx.Endorsements, 0, len(signatures))

	for _, sig := range signatures {
		sets = append(sets, &protoblocktx.Endorsements{
			EndorsementsWithIdentity: []*protoblocktx.EndorsementWithIdentity{{Endorsement: sig}},
		})
	}

	return sets
}

const (
	// CreatorCertificate denotes Creator field in protoblocktx.Identity to contain x509 certificate.
	CreatorCertificate = 0
	// CreatorID denotes Creator field in protoblocktx.Identity to contain the digest of x509 certificate.
	CreatorID = 1
)

// CreateEndorsementsForSignatureRule creates a EndorsementSet for a signature rule.
// It takes parallel slices of signatures, MSP IDs, and certificate bytes,
// and creates a EndorsementSet where each signature is paired with its corresponding
// identity (MSP ID and certificate). This is used when a set of signatures
// must all be present to satisfy a rule (e.g., an AND condition).
func CreateEndorsementsForSignatureRule(
	signatures, mspIDs, certBytesOrID [][]byte, creatorType int,
) *protoblocktx.Endorsements {
	set := &protoblocktx.Endorsements{
		EndorsementsWithIdentity: make([]*protoblocktx.EndorsementWithIdentity, 0, len(signatures)),
	}
	for i, sig := range signatures {
		eid := &protoblocktx.EndorsementWithIdentity{
			Endorsement: sig,
			Identity: &protoblocktx.Identity{
				MspId: string(mspIDs[i]),
			},
		}
		switch creatorType {
		case CreatorCertificate:
			eid.Identity.Creator = &protoblocktx.Identity_Certificate{Certificate: certBytesOrID[i]}
		case CreatorID:
			eid.Identity.Creator = &protoblocktx.Identity_CertificateId{
				CertificateId: hex.EncodeToString(certBytesOrID[i]),
			}
		}

		set.EndorsementsWithIdentity = append(set.EndorsementsWithIdentity, eid)
	}
	return set
}

// AppendToEndorsementSetsForThresholdRule is a utility function that creates new signature sets
// for a threshold rule and appends them to an existing slice of signature sets.
func AppendToEndorsementSetsForThresholdRule(
	ss []*protoblocktx.Endorsements, signatures ...[]byte,
) []*protoblocktx.Endorsements {
	return append(ss, CreateEndorsementsForThresholdRule(signatures...)...)
}
