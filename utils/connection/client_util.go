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
	maxMsgSize             = 100 * 1024 * 1024
	scResolverSchema       = "sc.connection"
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
)

var logger = logging.New("connection")

// DefaultGrpcRetryProfile defines the retry policy for a gRPC client connection.
var DefaultGrpcRetryProfile RetryProfile

var knownConnectionIssues = regexp.MustCompile(`(?i)EOF|connection\s+refused|closed\s+network\s+connection`)

// NewLoadBalancedDialConfig creates a dial config with load balancing between the endpoints
// in the given config.
func NewLoadBalancedDialConfig(config *ClientConfig) (*DialConfig, error) {
	tlsCredentials, err := config.Creds.ClientCredentials()
	if err != nil {
		return nil, err
	}
	return newLoadBalancedDialConfig(config.Endpoints, tlsCredentials, config.Retry), nil
}

// NewDialConfigPerEndpoint creates a list of dial configs; one for each endpoint in the given config.
func NewDialConfigPerEndpoint(config *ClientConfig) ([]*DialConfig, error) {
	tlsCreds, err := config.Creds.ClientCredentials()
	if err != nil {
		return nil, err
	}
	ret := make([]*DialConfig, len(config.Endpoints))
	for i, e := range config.Endpoints {
		ret[i] = newDialConfig(e.Address(), tlsCreds, config.Retry)
	}
	return ret, nil
}

// NewDialConfigWithCreds creates the default dial config with given transport credentials.
func NewDialConfigWithCreds(endpoint WithAddress, transportCredentials credentials.TransportCredentials) *DialConfig {
	return newDialConfig(endpoint.Address(), transportCredentials, &DefaultGrpcRetryProfile)
}

// NewInsecureDialConfig creates the default dial config with insecure credentials.
func NewInsecureDialConfig(endpoint WithAddress) *DialConfig {
	return newDialConfig(endpoint.Address(), insecure.NewCredentials(), &DefaultGrpcRetryProfile)
}

// NewInsecureLoadBalancedDialConfig creates the default dial config with insecure credentials.
func NewInsecureLoadBalancedDialConfig(endpoint []*Endpoint) *DialConfig {
	return newLoadBalancedDialConfig(endpoint, insecure.NewCredentials(), &DefaultGrpcRetryProfile)
}

// newLoadBalancedDialConfig creates a dial config with the default values for multiple endpoints.
func newLoadBalancedDialConfig(
	endpoint []*Endpoint, creds credentials.TransportCredentials, retry *RetryProfile,
) *DialConfig {
	resolverEndpoints := make([]resolver.Endpoint, len(endpoint))
	for i, e := range endpoint {
		resolverEndpoints[i] = resolver.Endpoint{Addresses: []resolver.Address{{Addr: e.Address()}}}
	}
	r := manual.NewBuilderWithScheme(scResolverSchema)
	r.UpdateState(resolver.State{Endpoints: resolverEndpoints})
	dialConfig := newDialConfig(fmt.Sprintf("%s:///%s", r.Scheme(), "method"), creds, retry)
	dialConfig.Resolver = r
	dialConfig.DialOpts = append(dialConfig.DialOpts, grpc.WithResolvers(r))
	return dialConfig
}

// newDialConfig creates a dial config with the default values.
func newDialConfig(
	address string, creds credentials.TransportCredentials, retry *RetryProfile,
) *DialConfig {
	return &DialConfig{
		Address: address,
		DialOpts: []grpc.DialOption{
			grpc.WithDefaultServiceConfig(retry.MakeGrpcRetryPolicyJSON()),
			grpc.WithTransportCredentials(creds),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(maxMsgSize),
				grpc.MaxCallSendMsgSize(maxMsgSize),
			),
			grpc.WithMaxCallAttempts(defaultGrpcMaxAttempts),
		},
	}
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
func OpenConnections[T WithAddress](
	endpoints []T,
	transportCredentials credentials.TransportCredentials,
) ([]*grpc.ClientConn, error) {
	logger.Infof("Opening connections to %d endpoints: %v.\n", len(endpoints), endpoints)
	connections := make([]*grpc.ClientConn, len(endpoints))
	for i, endpoint := range endpoints {
		conn, err := Connect(newDialConfig(endpoint.Address(), transportCredentials, &DefaultGrpcRetryProfile))
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
