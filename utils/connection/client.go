/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
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
	// MaxMsgSize is set to 100MB.
	MaxMsgSize       = 100 * 1024 * 1024
	scResolverSchema = "sc.connection"
)

type (
	// WithAddress represents any type that can generate an address.
	WithAddress interface {
		Address() string
	}

	// DialInfo contains the parameters to dial a connection.
	DialInfo struct {
		Endpoints []*Endpoint
		TLS       TLSCredentials
		Retry     *retry.Profile
	}

	// ClientParameters contain connection parameters.
	ClientParameters struct {
		Address        string
		Creds          credentials.TransportCredentials
		Retry          *retry.Profile
		AdditionalOpts []grpc.DialOption
	}
)

var logger = flogging.MustGetLogger("grpc-connection")

var knownConnectionIssues = regexp.MustCompile(
	`(?i)EOF|connection\s+refused|closed\s+network\s+connection|connection\s+reset`,
)

// NewDialInfo creates dial info from a client config.
func NewDialInfo(config *MultiClientConfig) (*DialInfo, error) {
	tls, err := NewClientTLSCredentials(config.TLS)
	if err != nil {
		return nil, err
	}
	return &DialInfo{
		Endpoints: config.Endpoints,
		Retry:     config.Retry,
		TLS:       *tls,
	}, nil
}

// NewLoadBalancedConnection creates a connection with load balancing between the endpoints
// in the given config.
func NewLoadBalancedConnection(config *MultiClientConfig) (*grpc.ClientConn, error) {
	d, err := NewDialInfo(config)
	if err != nil {
		return nil, err
	}
	return d.NewLoadBalancedConnection()
}

// NewConnectionPerEndpoint creates a list of connections; one for each endpoint in the given config.
func NewConnectionPerEndpoint(config *MultiClientConfig) ([]*grpc.ClientConn, error) {
	d, err := NewDialInfo(config)
	if err != nil {
		return nil, err
	}
	return d.NewConnectionPerEndpoint()
}

// NewLoadBalancedConnection creates a connection with load balancing between the endpoints.
func (d *DialInfo) NewLoadBalancedConnection() (*grpc.ClientConn, error) {
	tlsCredentials, err := NewClientGRPCTransportCredentials(&d.TLS)
	if err != nil {
		return nil, err
	}

	if len(d.Endpoints) == 1 {
		return NewConnection(ClientParameters{
			Address: d.Endpoints[0].Address(),
			Retry:   d.Retry,
			Creds:   tlsCredentials,
		})
	}

	resolverEndpoints := make([]resolver.Endpoint, len(d.Endpoints))
	for i, e := range d.Endpoints {
		// we're setting ServerName for each address because each service-instance has its own certificates.
		resolverEndpoints[i] = resolver.Endpoint{
			Addresses: []resolver.Address{{Addr: e.Address(), ServerName: e.Host}},
		}
	}
	r := manual.NewBuilderWithScheme(scResolverSchema)
	r.UpdateState(resolver.State{Endpoints: resolverEndpoints})

	// Create a meaningful target string for debugging by joining all endpoint addresses.
	targetName := AddressString(d.Endpoints...)
	return NewConnection(ClientParameters{
		Address:        fmt.Sprintf("%s:///%s", r.Scheme(), targetName),
		Creds:          tlsCredentials,
		Retry:          d.Retry,
		AdditionalOpts: []grpc.DialOption{grpc.WithResolvers(r)},
	})
}

// NewConnectionPerEndpoint creates a list of connections; one for each endpoint.
func (d *DialInfo) NewConnectionPerEndpoint() ([]*grpc.ClientConn, error) {
	tlsCreds, err := NewClientGRPCTransportCredentials(&d.TLS)
	if err != nil {
		return nil, err
	}
	connections := make([]*grpc.ClientConn, len(d.Endpoints))
	for i, e := range d.Endpoints {
		connections[i], err = NewConnection(ClientParameters{
			Address: e.Address(),
			Creds:   tlsCreds,
			Retry:   d.Retry,
		})
		if err != nil {
			CloseConnectionsLog(connections[:i]...)
			return nil, err
		}
	}
	return connections, nil
}

// NewSingleConnection creates a single connection given a client config.
func NewSingleConnection(config *ClientConfig) (*grpc.ClientConn, error) {
	tlsCreds, err := config.TLS.ClientCredentials()
	if err != nil {
		return nil, err
	}
	return NewConnection(ClientParameters{
		Address: config.Endpoint.Address(),
		Creds:   tlsCreds,
		Retry:   config.Retry,
	})
}

// NewConnection creates a connection with the given parameters.
// It will not attempt to create a connection with the remote.
func NewConnection(p ClientParameters) (*grpc.ClientConn, error) {
	dialOpts := append([]grpc.DialOption{
		grpc.WithDefaultServiceConfig(MakeGrpcRetryPolicyJSON(p.Retry)),
		grpc.WithTransportCredentials(p.Creds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(MaxMsgSize),
			grpc.MaxCallSendMsgSize(MaxMsgSize),
		),
		grpc.WithMaxCallAttempts(defaultGrpcMaxAttempts),
	}, p.AdditionalOpts...)

	cc, err := grpc.NewClient(p.Address, dialOpts...)
	if err != nil {
		logger.Errorf("Error opening connection to %s: %v", p.Address, err)
		return nil, errors.Wrap(err, "error connecting to grpc")
	}
	return cc, nil
}

// CloseConnections calls [closer.Close()] for all the given connections and return the close errors.
func CloseConnections[T io.Closer](connections ...T) error {
	logger.Infof("Closing %d connections.", len(connections))
	errs := make([]error, len(connections))
	for i, closer := range connections {
		v := reflect.ValueOf(closer)
		if !v.IsValid() || (v.Kind() == reflect.Ptr && v.IsNil()) {
			continue
		}
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

// FilterStreamRPCError filters RPC errors that caused due to ending stream.
func FilterStreamRPCError(rpcErr error) error {
	if rpcErr == nil {
		return nil
	}

	if isStreamEnd(rpcErr) {
		return nil
	}
	return rpcErr
}

// isStreamEnd returns true if an RPC error indicates stream end.
func isStreamEnd(rpcErr error) bool {
	if rpcErr == nil {
		return false
	}

	if isStreamContextEnd(rpcErr) {
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

// isStreamContextEnd returns true if an RPC error indicates stream context end.
func isStreamContextEnd(rpcErr error) bool {
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

// MakeGrpcRetryPolicyJSON defines the retry policy for a gRPC client connection.
// The retry policy applies to all subsequent gRPC calls made through the client connection.
// Our GRPC retry policy is applicable only for the following status codes:
//
//	(1) UNAVAILABLE	The service is currently unavailable (e.g., transient network issue, server down).
//	(2) DEADLINE_EXCEEDED	Operation took too long (deadline passed).
//	(3) RESOURCE_EXHAUSTED	Some resource (e.g., quota) has been exhausted; the operation cannot proceed.
func MakeGrpcRetryPolicyJSON(p *retry.Profile) string {
	p = p.WithDefaults()

	// We put limits on the values to ensure correct values.
	initialInterval := max(p.InitialInterval.Seconds(), time.Nanosecond.Seconds())
	maxInterval := max(p.MaxInterval.Seconds(), initialInterval)
	multiplier := max(p.Multiplier, 1.0001)
	maxElapsedTimeSeconds := max(p.MaxElapsedTime.Seconds(), maxInterval)
	ret := map[string]any{
		"loadBalancingConfig": []map[string]any{{
			"round_robin": make(map[string]any),
		}},
		"methodConfig": []map[string]any{{
			// Setting an empty name sets the default for all methods.
			"name": []any{make(map[string]any)},
			"retryPolicy": map[string]any{
				"maxAttempts":       CalcMaxAttempts(initialInterval, maxInterval, multiplier, maxElapsedTimeSeconds),
				"initialBackoff":    formatSeconds(initialInterval),
				"maxBackoff":        formatSeconds(maxInterval),
				"backoffMultiplier": multiplier,
				"retryableStatusCodes": []string{
					"UNAVAILABLE",
					"DEADLINE_EXCEEDED",
					"RESOURCE_EXHAUSTED",
				},
			},
		}},
	}
	jsonString, err := json.MarshalIndent(ret, "", "  ")
	if err != nil {
		logger.Warnf("failed to marshal retry profile to JSON: %s", err)
		return "{}"
	}
	return string(jsonString)
}

// CalcMaxAttempts calculates the number of attempts given the following parameters:
// - initialInterval > 0
// - maxInterval     >= i
// - multiplier      > 1
// - maxElapsedTime > i.
func CalcMaxAttempts(initialInterval, maxInterval, multiplier, maxElapsedTime float64) int {
	nextBackoffInterval := initialInterval
	var estimatedElapsedTime float64
	var attempts int
	for attempts = 0; estimatedElapsedTime <= maxElapsedTime; attempts++ {
		estimatedElapsedTime += nextBackoffInterval
		nextBackoffInterval = min(nextBackoffInterval*multiplier, maxInterval)
	}
	return attempts
}

func formatSeconds(sec float64) string {
	return fmt.Sprintf("%ss", strconv.FormatFloat(sec, 'f', -1, 64))
}
