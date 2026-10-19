/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

func main() {
	if err := checkAnchor(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func checkAnchor(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: freezecheck <coordinator-endpoint> <next-block>")
	}
	expected, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid next block: %w", err)
	}
	tlsCredentials, err := connection.NewClientTLSCredentials(connection.TLSConfig{
		Mode:        connection.MutualTLSMode,
		CertPath:    "/client-tls/server.crt",
		KeyPath:     "/client-tls/server.key",
		CACertPaths: []string{"/org-tls-ca.pem"},
	})
	if err != nil {
		return err
	}
	transportCredentials, err := connection.NewClientGRPCTransportCredentials(tlsCredentials)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(args[0], grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	next, err := servicepb.NewCoordinatorClient(conn).GetNextBlockNumberToCommit(ctx, &emptypb.Empty{}, grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	if next.Number != expected {
		return fmt.Errorf("coordinator expects block %d, want %d", next.Number, expected)
	}
	return nil
}
