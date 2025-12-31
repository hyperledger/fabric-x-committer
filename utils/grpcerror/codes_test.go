/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package grpcerror

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestHasCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		inputErr       error
		inputCode      codes.Code
		expectedReturn bool
	}{
		{
			name:           "nil error returns false",
			inputErr:       nil,
			expectedReturn: false,
		},
		{
			name:           "non-status error returns false",
			inputErr:       errors.New("plain error"),
			inputCode:      codes.Internal,
			expectedReturn: false,
		},
		{
			name:           "status error with matching code returns true",
			inputErr:       status.Error(codes.Internal, "internal error occurred"),
			inputCode:      codes.Internal,
			expectedReturn: true,
		},
		{
			name:           "status error with mismatched code returns false",
			inputErr:       status.Error(codes.NotFound, "not found error occurred"),
			inputCode:      codes.Internal,
			expectedReturn: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.expectedReturn, HasCode(tc.inputErr, tc.inputCode))
		})
	}
}

func TestHasCodeWithGRPCService(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	server := test.StartGrpcServersForTest(ctx, t, 1, func(server *grpc.Server, _ int) {
		healthgrpc.RegisterHealthServer(server, &healthgrpc.UnimplementedHealthServer{})
	}, test.InsecureTLSConfig)

	conn := test.NewInsecureConnectionWithRetry(t, &server.Configs[0].Endpoint, connection.RetryProfile{
		MaxElapsedTime: 2 * time.Second,
	})

	client := healthgrpc.NewHealthClient(conn)

	_, err := client.Check(ctx, nil)
	require.True(t, HasCode(err, codes.Unimplemented)) // all APIs are codes.Unimplemented

	_, err = client.List(ctx, nil)
	require.False(t, HasCode(err, codes.NotFound)) // all APIs are codes.Unimplemented

	server.Servers[0].Stop()
	test.CheckServerStopped(t, server.Configs[0].Endpoint.Address())

	_, err = client.Check(ctx, nil)
	require.Truef(t, HasCode(err, codes.Unavailable), "code: %s", GetCode(err))
	require.NoError(t, FilterUnavailableErrorCode(err))
}

func TestWrapErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		createFunc       func(error) error
		input            error
		expectedNilError bool
		expectedCode     codes.Code
		expectedMsg      string
	}{
		{
			name:             "WrapInternalError returns nil for nil input",
			createFunc:       WrapInternalError,
			input:            nil,
			expectedNilError: true,
		},
		{
			name:             "WrapInternalError returns error with Internal code",
			createFunc:       WrapInternalError,
			input:            errors.New("something went wrong"),
			expectedNilError: false,
			expectedCode:     codes.Internal,
			expectedMsg:      "something went wrong",
		},
		{
			name:             "WrapInvalidArgument returns nil for nil input",
			createFunc:       WrapInvalidArgument,
			input:            nil,
			expectedNilError: true,
		},
		{
			name:             "WrapInvalidArgument returns error with InvalidArgument code",
			createFunc:       WrapInvalidArgument,
			input:            errors.New("invalid argument provided"),
			expectedNilError: false,
			expectedCode:     codes.InvalidArgument,
			expectedMsg:      "invalid argument provided",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.createFunc(tc.input)
			if tc.expectedNilError {
				require.NoError(t, err)
				return
			}

			st, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, tc.expectedCode, st.Code())
			require.Equal(t, tc.expectedMsg, st.Message())
		})
	}
}
