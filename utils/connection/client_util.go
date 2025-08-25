/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"

	"github.com/cockroachdb/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

const (
	// Connected indicates that the connection to the service is currently established.
	Connected = 1.0
	// Disconnected indicates that the connection to the service is currently not established.
	Disconnected = 0

	// defaultGrpcMaxAttempts is set to a high number to allow the timeout to dictate the retry end condition.
	defaultGrpcMaxAttempts = 1024
	// TODO: All services including the orderer must use the same default maximum message size.
	//       Hence, we need to move this constant to fabric-x-common.
	maxMsgSize       = 100 * 1024 * 1024
	scResolverSchema = "sc.connection"
)

type (
	// DialConfig represents the dial options to create a connection.
	DialConfig struct {
		Address  string
		DialOpts []grpc.DialOption
		// Resolver may be updated to update the connection's endpoints.
		Resolver *manual.Resolver
	}

	// WithAddress represents any type that can generate an address.
	WithAddress interface {
		Address() string
	}

	// DialConfigBuilder builds a DialConfig.
	DialConfigBuilder struct {
		address  string
		creds    credentials.TransportCredentials
		retry    *RetryProfile
		resolver *manual.Resolver
	}
)

var logger = logging.New("connection")

var knownConnectionIssues = regexp.MustCompile(`(?i)EOF|connection\s+refused|closed\s+network\s+connection`)

// NewDialConfigBuilder starts building a new DialConfig.
func NewDialConfigBuilder(address string) *DialConfigBuilder {
	return &DialConfigBuilder{
		address: address,
	}
}

// WithCredentials attach credentials.TransportCredentials to the builder.
func (b *DialConfigBuilder) WithCredentials(creds credentials.TransportCredentials) *DialConfigBuilder {
	b.creds = creds
	return b
}

// WithRetry attach retry profile to the builder.
func (b *DialConfigBuilder) WithRetry(retry *RetryProfile) *DialConfigBuilder {
	b.retry = retry
	return b
}

// WithResolver attach a manual.resolver to the builder.
func (b *DialConfigBuilder) WithResolver(res *manual.Resolver) *DialConfigBuilder {
	b.resolver = res
	return b
}

// Build builds and returns the dial config.
func (b *DialConfigBuilder) Build() *DialConfig {
	if b.creds == nil {
		b.creds = insecure.NewCredentials()
	}

	dialConfig := &DialConfig{
		Address: b.address,
		DialOpts: []grpc.DialOption{
			grpc.WithDefaultServiceConfig(b.retry.MakeGrpcRetryPolicyJSON()),
			grpc.WithTransportCredentials(b.creds),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(maxMsgSize),
				grpc.MaxCallSendMsgSize(maxMsgSize),
			),
			grpc.WithMaxCallAttempts(defaultGrpcMaxAttempts),
		},
	}

	if b.resolver != nil {
		dialConfig.Resolver = b.resolver
		dialConfig.DialOpts = append(dialConfig.DialOpts, grpc.WithResolvers(b.resolver))
	}

	return dialConfig
}

// NewLoadBalancedDialConfig creates a dial config with load balancing between the endpoints
// in the given config.
func NewLoadBalancedDialConfig(config MultiClientConfig) (*DialConfig, error) {
	tlsCredentials, err := config.TLS.ClientCredentials()
	if err != nil {
		return nil, err
	}

	resolverEndpoints := make([]resolver.Endpoint, len(config.Endpoints))
	for i, e := range config.Endpoints {
		// we're setting ServerName for each address because each service-instance has its own certificates.
		resolverEndpoints[i] = resolver.Endpoint{
			Addresses: []resolver.Address{{Addr: e.Address(), ServerName: e.Host}},
		}
	}
	r := manual.NewBuilderWithScheme(scResolverSchema)
	r.UpdateState(resolver.State{Endpoints: resolverEndpoints})

	return NewDialConfigBuilder(fmt.Sprintf("%s:///%s", r.Scheme(), "method")).
		WithCredentials(tlsCredentials).
		WithRetry(config.Retry).
		WithResolver(r).
		Build(), nil
}

// NewDialConfigPerEndpoint creates a list of dial configs; one for each endpoint in the given config.
func NewDialConfigPerEndpoint(config *MultiClientConfig) ([]*DialConfig, error) {
	tlsCreds, err := config.TLS.ClientCredentials()
	if err != nil {
		return nil, err
	}
	ret := make([]*DialConfig, len(config.Endpoints))
	for i, e := range config.Endpoints {
		ret[i] = NewDialConfigBuilder(e.Address()).
			WithCredentials(tlsCreds).
			WithRetry(config.Retry).
			Build()
	}
	return ret, nil
}

// NewSingleDialConfig creates a single dial config given a client config.
func NewSingleDialConfig(config *ClientConfig) (*DialConfig, error) {
	tlsCreds, err := config.TLS.ClientCredentials()
	if err != nil {
		return nil, err
	}
	return NewDialConfigBuilder(config.Endpoint.Address()).
		WithCredentials(tlsCreds).
		WithRetry(config.Retry).
		Build(), nil
}

// SetRetryProfile replaces the GRPC retry policy.
func (d *DialConfig) SetRetryProfile(profile *RetryProfile) {
	// Index 0 is reserved for the GRPC retry policy.
	d.DialOpts[0] = grpc.WithDefaultServiceConfig(profile.MakeGrpcRetryPolicyJSON())
}

// CloseConnections calls [closer.Close()] for all the given connections and return the close errors.
func CloseConnections[T io.Closer](connections ...T) error {
	logger.Infof("Closing %d connections.", len(connections))
	errs := make([]error, len(connections))
	for i, closer := range connections {
		errs[i] = filterAcceptableCloseErr(closer.Close())
	}
	return errors.Join(errs...)
}

// CloseConnectionsLog calls [closer.Close()] for all the given connections and log the close errors.
func CloseConnectionsLog[T io.Closer](connections ...T) {
	closeErr := CloseConnections(connections...)
	if closeErr != nil {
		logger.Errorf("failed closing connections: %v", closeErr)
	}
}

var acceptableCloseErr = []error{
	io.EOF,
	io.ErrClosedPipe,
	io.ErrUnexpectedEOF,
	net.ErrClosed,
	context.Canceled,
	context.DeadlineExceeded,
}

func filterAcceptableCloseErr(err error) error {
	if err == nil {
		return nil
	}
	for _, e := range acceptableCloseErr {
		if errors.Is(err, e) {
			return nil
		}
	}
	return err
}

// Connect creates a new [grpc.ClientConn] with the given [DialConfig].
// It will not attempt to create a connection with the remote.
func Connect(config *DialConfig) (*grpc.ClientConn, error) {
	cc, err := grpc.NewClient(config.Address, config.DialOpts...)
	if err != nil {
		logger.Errorf("Error openning connection to %s: %v", config.Address, err)
		return nil, errors.Wrap(err, "error connecting to grpc")
	}
	return cc, nil
}

// OpenConnections opens connections with multiple remotes.
func OpenConnections(config MultiClientConfig) ([]*grpc.ClientConn, error) {
	logger.Infof("Opening connections to %d endpoints: %v.\n", len(config.Endpoints), config.Endpoints)
	dialConfigs, err := NewDialConfigPerEndpoint(&config)
	if err != nil {
		return nil, errors.Wrapf(err, "error while creating dial configs")
	}
	connections := make([]*grpc.ClientConn, len(config.Endpoints))
	for i, dial := range dialConfigs {
		conn, err := Connect(dial)
		if err != nil {
			logger.Errorf("Error connecting: %v", err)
			CloseConnectionsLog(connections[:i]...)
			return nil, err
		}
		connections[i] = conn
	}
	logger.Infof("Opened %d connections", len(connections))
	return connections, nil
}

// FilterStreamRPCError filters RPC errors that caused due to ending stream.
func FilterStreamRPCError(rpcErr error) error {
	if rpcErr == nil {
		return nil
	}

	if IsStreamEnd(rpcErr) {
		return nil
	}
	return rpcErr
}

// IsStreamEnd returns true if an RPC error indicates stream end.
func IsStreamEnd(rpcErr error) bool {
	if rpcErr == nil {
		return false
	}

	if IsStreamContextEnd(rpcErr) {
		return true
	}

	errStatus, ok := status.FromError(rpcErr)
	if !ok {
		return errors.Is(rpcErr, io.EOF)
	}

	rpcErrCode := errStatus.Code()
	if rpcErrCode == codes.Unavailable && knownConnectionIssues.MatchString(errStatus.Message()) {
		return true
	}

	return false
}

// IsStreamContextEnd returns true if an RPC error indicates stream context end.
func IsStreamContextEnd(rpcErr error) bool {
	if rpcErr == nil {
		return false
	}

	errStatus, ok := status.FromError(rpcErr)
	if !ok {
		return errors.Is(rpcErr, context.Canceled) || errors.Is(rpcErr, context.DeadlineExceeded)
	}

	rpcErrCode := errStatus.Code()
	if rpcErrCode == codes.Canceled || rpcErrCode == codes.DeadlineExceeded {
		return true
	}

	return false
}

// AddressString returns the addresses as a string with comma as a separator between them.
func AddressString[T WithAddress](addresses ...T) string {
	listOfAddresses := make([]string, len(addresses))
	for i, address := range addresses {
		listOfAddresses[i] = address.Address()
	}
	return strings.Join(listOfAddresses, ",")
}
