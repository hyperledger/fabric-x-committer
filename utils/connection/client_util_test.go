/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

//nolint:paralleltest // modifies grpc logger.
func TestGRPCRetry(t *testing.T) {
	// We start GRPC logging for this test to allow visibility into the retry process.
	// This change prevents us from running this test in parallel to others.
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(os.Stderr, io.Discard, os.Stderr))
	t.Cleanup(func() {
		grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, os.Stderr))
	})

	t.Log("Starting service")
	serverConfig := connection.NewLocalHostServer(test.InsecureTLSConfig)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	vcGrpc := test.RunGrpcServerForTest(ctx, t, serverConfig, mock.NewMockVcService().RegisterService)

	t.Log("Connecting")
	conn := test.NewInsecureConnection(t, &serverConfig.Endpoint)
	client := servicepb.NewValidationAndCommitServiceClient(conn)

	t.Log("Sanity check")
	_, err := client.GetNamespacePolicies(ctx, nil)
	require.NoError(t, err)

	t.Log("Stopping the grpc server")
	cancel()
	vcGrpc.Stop()
	test.CheckServerStopped(t, serverConfig.Endpoint.Address())

	// We override the context to avoid mistakenly using the previous one.
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	go func() {
		time.Sleep(30 * time.Second)
		t.Log("Service is starting")
		test.RunGrpcServerForTest(ctx, t, serverConfig, mock.NewMockVcService().RegisterService)
	}()

	t.Log("Attempting to connect with default GRPC config")
	_, err = client.GetNamespacePolicies(ctx, nil)
	require.NoError(t, err)

	conn2 := test.NewInsecureConnectionWithRetry(t, &serverConfig.Endpoint, connection.RetryProfile{
		MaxElapsedTime: 2 * time.Second,
	})
	client2 := servicepb.NewValidationAndCommitServiceClient(conn2)

	t.Log("Sanity check with lower timeout")
	_, err = client2.GetNamespacePolicies(ctx, nil)
	require.NoError(t, err)

	t.Log("Stopping the grpc server")
	cancel()
	vcGrpc.Stop()
	test.CheckServerStopped(t, serverConfig.Endpoint.Address())

	// We override the context to avoid mistakenly using the previous one.
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	go func() {
		time.Sleep(30 * time.Second)
		t.Log("Service is starting")
		test.RunGrpcServerForTest(ctx, t, serverConfig, mock.NewMockVcService().RegisterService)
	}()

	t.Log("Attempting to connect again with lower timeout")
	_, err = client2.GetNamespacePolicies(ctx, nil)
	require.Error(t, err)
}

//nolint:paralleltest // modifies grpc logger.
func TestGRPCRetryMultiEndpoints(t *testing.T) {
	// We start GRPC logging for this test to allow visibility into the retry process.
	// This change prevents us from running this test in parallel to others.
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(os.Stderr, io.Discard, os.Stderr))
	t.Cleanup(func() {
		grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, os.Stderr))
	})

	t.Log("Starting service")
	serverConfig := connection.NewLocalHostServer(test.InsecureTLSConfig)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	test.RunGrpcServerForTest(ctx, t, serverConfig, mock.NewMockVcService().RegisterService)

	t.Log("Connecting")
	conn := test.NewInsecureConnection(t, &serverConfig.Endpoint)
	client := servicepb.NewValidationAndCommitServiceClient(conn)

	t.Log("Sanity check")
	_, err := client.GetNamespacePolicies(ctx, nil)
	require.NoError(t, err)

	t.Log("Creating fake service address")
	fakeServerConfig := connection.NewLocalHostServer(test.InsecureTLSConfig)
	l, err := fakeServerConfig.Listener(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		connection.CloseConnectionsLog(l)
	})

	t.Log("Setup dial config for multiple endpoints")

	t.Log("Connecting to multiple endpoints")
	multiConn := test.NewInsecureLoadBalancedConnection(t, []*connection.Endpoint{
		// We put the fake one first to ensure we iterate over it.
		&fakeServerConfig.Endpoint,
		&serverConfig.Endpoint,
	})
	multiClient := servicepb.NewValidationAndCommitServiceClient(multiConn)
	for i := range 100 {
		t.Logf("Fetch attempt: %d", i)
		_, err = multiClient.GetNamespacePolicies(ctx, nil)
		require.NoError(t, err)
	}
}

type fakeBroadcastDeliver struct{}

func (fakeBroadcastDeliver) Deliver(stream peer.Deliver_DeliverServer) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		switch len(msg.Payload) {
		case 0:
			return errors.New("bad env")
		case 1:
			return nil
		}
		err = stream.Send(&peer.DeliverResponse{})
		if err != nil {
			return err
		}
	}
}

func (fakeBroadcastDeliver) DeliverFiltered(peer.Deliver_DeliverFilteredServer) error {
	panic("not implemented")
}

func (fakeBroadcastDeliver) DeliverWithPrivateData(peer.Deliver_DeliverWithPrivateDataServer) error {
	panic("not implemented")
}

var (
	badEnv = &common.Envelope{}
	endEnv = &common.Envelope{
		Payload: make([]byte, 1),
	}
	goodEnv = &common.Envelope{
		Payload: make([]byte, 2),
	}
)

type filterTestEnv struct {
	service       *fakeBroadcastDeliver
	serverConf    *connection.ServerConfig
	server        *grpc.Server
	client        peer.DeliverClient
	deliver       peer.Deliver_DeliverClient
	serviceCancel context.CancelFunc
	clientCancel  context.CancelFunc
}

func newFilterTestEnv(t *testing.T) *filterTestEnv {
	t.Helper()
	env := &filterTestEnv{
		service:    &fakeBroadcastDeliver{},
		serverConf: &connection.ServerConfig{Endpoint: connection.Endpoint{Host: "localhost"}},
	}

	serviceCtx, serviceCancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(serviceCancel)
	env.serviceCancel = serviceCancel

	env.server = test.RunGrpcServerForTest(serviceCtx, t, env.serverConf, func(server *grpc.Server) {
		peer.RegisterDeliverServer(server, env.service)
	})
	conn := test.NewInsecureConnection(t, &env.serverConf.Endpoint)
	env.client = peer.NewDeliverClient(conn)

	clientCtx, clientCancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(clientCancel)
	env.clientCancel = clientCancel
	var err error
	env.deliver, err = env.client.Deliver(clientCtx)
	require.NoError(t, err)

	// Sanity check.
	err = env.deliver.Send(goodEnv)
	require.NoError(t, err)
	_, err = env.deliver.Recv()
	require.NoError(t, err)
	requireNoWrappedError(t, err)

	return env
}

func TestFilterStreamRPCError(t *testing.T) {
	t.Parallel()

	t.Run("EOF", func(t *testing.T) {
		t.Parallel()
		env := newFilterTestEnv(t)
		err := env.deliver.Send(endEnv)
		require.NoError(t, err)
		_, err = env.deliver.Recv()
		require.ErrorIs(t, err, io.EOF)
		requireNoWrappedError(t, err)
	})

	t.Run("client ctx cancel", func(t *testing.T) {
		t.Parallel()
		env := newFilterTestEnv(t)
		env.clientCancel()
		_, err := env.deliver.Recv()
		require.Error(t, err)
		requireNoWrappedError(t, err)
		requireErrorIsRPC(t, err, codes.Canceled)
	})

	t.Run("client ctx timeout", func(t *testing.T) {
		t.Parallel()
		clientCtx, clientCancel := context.WithTimeout(t.Context(), time.Second)
		t.Cleanup(clientCancel)
		env := newFilterTestEnv(t)
		deliver, err := env.client.Deliver(clientCtx)
		require.NoError(t, err)
		time.Sleep(time.Second)
		_, err = deliver.Recv()
		require.Error(t, err)
		requireNoWrappedError(t, err)
		requireErrorIsRPC(t, err, codes.DeadlineExceeded)
	})

	t.Run("with error", func(t *testing.T) {
		t.Parallel()
		env := newFilterTestEnv(t)
		err := env.deliver.Send(badEnv)
		require.NoError(t, err)
		_, err = env.deliver.Recv()
		require.Error(t, err)
		require.Error(t, connection.FilterStreamRPCError(err))
		require.Error(t, connection.FilterStreamRPCError(errors.Join(err, errors.New("failed"))))
		require.Error(t, connection.FilterStreamRPCError(fmt.Errorf("failed: %w", err)))
	})

	t.Run("server ctx cancel", func(t *testing.T) {
		t.Parallel()
		env := newFilterTestEnv(t)
		go func() {
			time.Sleep(3 * time.Second)
			env.serviceCancel()
		}()

		_, err := env.deliver.Recv()
		require.Error(t, err)
		// This returns either codes.Canceled or codes.Unavailable (EOF).
		requireNoWrappedError(t, err)
	})

	t.Run("server shutdown", func(t *testing.T) {
		t.Parallel()
		env := newFilterTestEnv(t)
		go func() {
			time.Sleep(3 * time.Second)
			env.server.Stop()
		}()
		_, err := env.deliver.Recv()
		require.Error(t, err)
		// This returns either codes.Canceled or codes.Unavailable (EOF).
		requireNoWrappedError(t, err)
	})
}

func requireNoWrappedError(t *testing.T, err error) {
	t.Helper()
	require.NoError(t, connection.FilterStreamRPCError(err))
	if err == nil {
		return
	}
	require.NoError(t, connection.FilterStreamRPCError(errors.Join(err, errors.New("failed"))))
	require.NoError(t, connection.FilterStreamRPCError(fmt.Errorf("failed: %w", err)))
}

func requireErrorIsRPC(t *testing.T, rpcErr error, code codes.Code) {
	t.Helper()
	errStatus, ok := status.FromError(rpcErr)
	require.True(t, ok)
	rpcErrCode := errStatus.Code()
	require.Equal(t, code, rpcErrCode)
}

type closer struct {
	err error
}

func (c *closer) Close() error {
	return c.err
}

func TestCloseConnections(t *testing.T) {
	t.Parallel()
	testErrors := []error{
		io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, net.ErrClosed, context.Canceled, context.DeadlineExceeded,
	}
	for _, err := range testErrors {
		t.Run(err.Error(), func(t *testing.T) {
			t.Parallel()
			require.NoError(t, connection.CloseConnections(&closer{err: errors.Wrap(err, "failed")}))
		})
	}

	t.Run("all", func(t *testing.T) {
		t.Parallel()
		closers := make([]*closer, len(testErrors))
		for i, err := range testErrors {
			closers[i] = &closer{err: errors.Wrap(err, "failed")}
		}
		require.NoError(t, connection.CloseConnections(closers...))
	})
}
