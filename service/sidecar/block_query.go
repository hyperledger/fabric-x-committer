/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"strings"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/common/ledger/blkstorage"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
)

// blockQuery implements committerpb.BlockQueryServiceServer by delegating
// read-only queries directly to the underlying block store.
type blockQuery struct {
	committerpb.UnimplementedBlockQueryServiceServer
	store *blkstorage.BlockStore
}

func newBlockQuery(store *blkstorage.BlockStore) *blockQuery {
	return &blockQuery{store: store}
}

// GetBlockchainInfo returns the current blockchain height and hash metadata.
func (s *blockQuery) GetBlockchainInfo(_ context.Context, _ *emptypb.Empty) (*common.BlockchainInfo, error) {
	info, err := s.store.GetBlockchainInfo()
	if err != nil {
		logger.Errorf("GetBlockchainInfo failed: %v", err)
		return nil, grpcerror.WrapInternalError(err)
	}
	return info, nil
}

// GetBlockByNumber retrieves a block by its sequence number.
func (s *blockQuery) GetBlockByNumber(_ context.Context, req *committerpb.BlockNumber) (*common.Block, error) {
	block, err := s.store.RetrieveBlockByNumber(req.GetNumber())
	if err != nil {
		return nil, wrapQueryError(err)
	}
	return block, nil
}

// GetBlockByTxID retrieves the block that contains the specified transaction.
func (s *blockQuery) GetBlockByTxID(_ context.Context, req *committerpb.TxID) (*common.Block, error) {
	block, err := s.store.RetrieveBlockByTxID(req.GetTxId())
	if err != nil {
		return nil, wrapQueryError(err)
	}
	return block, nil
}

// GetTxByID retrieves the transaction envelope for the specified transaction ID.
func (s *blockQuery) GetTxByID(_ context.Context, req *committerpb.TxID) (*common.Envelope, error) {
	envelope, err := s.store.RetrieveTxByID(req.GetTxId())
	if err != nil {
		return nil, wrapQueryError(err)
	}
	return envelope, nil
}

// NOTE: This is fragile — we rely on matching error message substrings ("no such",
// "not available") because the blkstorage package does not return a typed NotFound
// error. If the blockstore changes its error messages, this will silently misclassify
// not-found errors as internal errors. The proper fix is to update the blockstore to
// return a sentinel error type (e.g., blkstorage.ErrNotFound) that we can match with
// errors.Is instead of string matching.
func wrapQueryError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "no such") || strings.Contains(msg, "not available") {
		return grpcerror.WrapNotFound(err)
	}
	logger.Errorf("Unexpected block store error: %v", err)
	return grpcerror.WrapInternalError(err)
}
