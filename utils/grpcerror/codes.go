/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package grpcerror

import (
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

var logger = logging.New("grpcerror")

// HasCode returns true if the rpcErr holds the given code.
func HasCode(rpcErr error, code codes.Code) bool {
	if rpcErr == nil {
		return false
	}
	return GetCode(rpcErr) == code
}

// GetCode returns GRPC error code.
func GetCode(rpcErr error) codes.Code {
	if rpcErr == nil {
		return codes.OK
	}

	errStatus, ok := status.FromError(rpcErr)
	if !ok {
		return codes.OK
	}

	return errStatus.Code()
}

// WrapInternalError creates a grpc error with a [codes.Internal] status code for a given error.
func WrapInternalError(err error) error {
	return wrap(codes.Internal, err)
}

// WrapInvalidArgument creates a grpc error with a [codes.InvalidArgument] status code for a given error.
func WrapInvalidArgument(err error) error {
	return wrap(codes.InvalidArgument, err)
}

// WrapCancelled creates a grpc error with a [codes.Canceled] status code for a given error.
func WrapCancelled(err error) error {
	return wrap(codes.Canceled, err)
}

// WrapFailedPrecondition creates a grpc error with a [codes.FailedPrecondition] status code for a given error.
func WrapFailedPrecondition(err error) error {
	return wrap(codes.FailedPrecondition, err)
}

// WrapUnimplemented creates a grpc error with a [codes.Unimplemented] status code for a given error.
func WrapUnimplemented(err error) error {
	return wrap(codes.Unimplemented, err)
}

// WrapNotFound creates a grpc error with a [codes.NotFound] status code for a given error.
func WrapNotFound(err error) error {
	return wrap(codes.NotFound, err)
}

func wrap(c codes.Code, err error) error {
	if err == nil {
		return nil
	}
	return status.Error(c, err.Error())
}

// FilterUnavailableErrorCode rpc error that caused due to transient connectivity issue.
func FilterUnavailableErrorCode(rpcErr error) error {
	code := GetCode(rpcErr)
	if slices.Contains([]codes.Code{codes.Unavailable, codes.DeadlineExceeded}, code) {
		return nil
	}
	return rpcErr
}

// WrapWithContext wraps an error from a downstream gRPC call while preserving the original
// status code if present. This is useful for middleware services that call other gRPC services
// and need to pass through errors to upstream clients.
// If the error is a gRPC status error, it logs the status code and message, preserves the status
// code and adds context to the message.
// If the error is not a gRPC status error (e.g., network timeout), it wraps it as Internal.
func WrapWithContext(err error, context string) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if ok {
		logger.Errorf("%s: gRPC status code=%s, message=%s", context, st.Code(), st.Message())
		return status.Errorf(st.Code(), "%s: %s", context, st.Message())
	}

	return status.Errorf(codes.Internal, "%s: %v", context, err)
}
