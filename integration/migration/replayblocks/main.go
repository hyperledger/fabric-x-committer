/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

func main() {
	if err := replayBlocks(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func replayBlocks(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: replayblocks <coordinator-endpoint> <block> [block...]")
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := servicepb.NewCoordinatorClient(conn)
	stream, err := client.BlockProcessing(ctx, grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	for _, path := range args[1:] {
		blockBytes, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		block := &common.Block{}
		if unmarshalErr := proto.Unmarshal(blockBytes, block); unmarshalErr != nil {
			return unmarshalErr
		}
		batch, mapErr := sidecar.MapBlockForTest(block)
		if mapErr != nil {
			return mapErr
		}
		if sendErr := stream.Send(batch); sendErr != nil {
			return sendErr
		}
		for remaining := len(batch.Txs) + len(batch.Rejected); remaining > 0; {
			statuses, recvErr := stream.Recv()
			if recvErr != nil {
				return recvErr
			}
			for _, status := range statuses.Status {
				if status.Status != committerpb.Status_COMMITTED {
					return fmt.Errorf("block %d transaction %s: %s", block.Header.Number, status.Ref.TxId, status.Status.String())
				}
			}
			remaining -= len(statuses.Status)
		}
		if _, setErr := client.SetLastCommittedBlockNumber(ctx, &servicepb.BlockRef{Number: block.Header.Number}); setErr != nil {
			return setErr
		}
	}
	return stream.CloseSend()
}
