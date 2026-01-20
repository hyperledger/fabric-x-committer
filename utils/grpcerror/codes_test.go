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
	server := test.StartGrpcServersForTest(ctx, t, 1, func(server *grpc.Server) {
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
		{
			name:             "WrapCancelled returns nil for nil input",
			createFunc:       WrapCancelled,
			input:            nil,
			expectedNilError: true,
		},
		{
			name:             "WrapCancelled returns error with Canceled code",
			createFunc:       WrapCancelled,
			input:            errors.New("operation cancelled"),
			expectedNilError: false,
			expectedCode:     codes.Canceled,
			expectedMsg:      "operation cancelled",
		},
		{
			name:             "WrapFailedPrecondition returns nil for nil input",
			createFunc:       WrapFailedPrecondition,
			input:            nil,
			expectedNilError: true,
		},
		{
			name:             "WrapFailedPrecondition returns error with FailedPrecondition code",
			createFunc:       WrapFailedPrecondition,
			input:            errors.New("system not in required state"),
			expectedNilError: false,
			expectedCode:     codes.FailedPrecondition,
			expectedMsg:      "system not in required state",
		},
		{
			name:             "WrapUnimplemented returns nil for nil input",
			createFunc:       WrapUnimplemented,
			input:            nil,
			expectedNilError: true,
		},
		{
			name:             "WrapUnimplemented returns error with Unimplemented code",
			createFunc:       WrapUnimplemented,
			input:            errors.New("method is deprecated"),
			expectedNilError: false,
			expectedCode:     codes.Unimplemented,
			expectedMsg:      "method is deprecated",
		},
		{
			name:             "WrapNotFound returns nil for nil input",
			createFunc:       WrapNotFound,
			input:            nil,
			expectedNilError: true,
		},
		{
			name:             "WrapNotFound returns error with NotFound code",
			createFunc:       WrapNotFound,
			input:            errors.New("resource not found"),
			expectedNilError: false,
			expectedCode:     codes.NotFound,
			expectedMsg:      "resource not found",
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

func TestWrapWithContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		input        error
		context      string
		expectedNil  bool
		expectedCode codes.Code
		expectedMsg  string
	}{
		{
			name:        "nil error returns nil",
			input:       nil,
			context:     "calling downstream",
			expectedNil: true,
		},
		{
			name:         "gRPC status error preserves code and adds context",
			input:        status.Error(codes.NotFound, "user not found"),
			context:      "failed to get user",
			expectedNil:  false,
			expectedCode: codes.NotFound,
			expectedMsg:  "failed to get user: user not found",
		},
		{
			name:         "gRPC status error with different code preserves that code",
			input:        status.Error(codes.PermissionDenied, "access denied"),
			context:      "authorization check",
			expectedNil:  false,
			expectedCode: codes.PermissionDenied,
			expectedMsg:  "authorization check: access denied",
		},
		{
			name:         "non-gRPC error wraps as Internal",
			input:        errors.New("network timeout"),
			context:      "calling service B",
			expectedNil:  false,
			expectedCode: codes.Internal,
			expectedMsg:  "calling service B: network timeout",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := WrapWithContext(tc.input, tc.context)
			if tc.expectedNil {
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
