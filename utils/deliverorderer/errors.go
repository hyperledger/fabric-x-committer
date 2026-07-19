/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/utils/retry"
)

var (
	// Stream restart errors with labels for metrics.
	errRequireRestart = errors.New("stream restart required")
	errConfigUpdate   = &labeledError{
		err:   errors.Wrap(errRequireRestart, "config block received"),
		label: "config_update",
	}
	errSuspicion = &labeledError{
		err:   errors.Wrap(errRequireRestart, "block withholding suspicion"),
		label: "suspicion",
	}
	errDataBlockError = &labeledError{
		err:   errors.Wrap(retry.ErrBackOff, "data block error"),
		label: "data_block_error",
	}

	// Verification errors with metrics labels.

	// ErrUnexpectedBlockNumber is returned when a block arrives out of order (higher than expected).
	ErrUnexpectedBlockNumber = &labeledError{
		err:   errors.New("received unexpected block number"),
		label: "unexpected_block_number",
	}
	// ErrDuplicateBlock is returned when a block that was already processed arrives again.
	ErrDuplicateBlock = &labeledError{
		err:   errors.New("received duplicate block"),
		label: "duplicate_block",
	}
	// ErrMalformedBlock is returned when a block structure is invalid.
	ErrMalformedBlock = &labeledError{
		err:   errors.New("received malformed block"),
		label: "malformed_block",
	}
	// ErrHeaderHashMismatch is returned when the previous block header hash doesn't match.
	ErrHeaderHashMismatch = &labeledError{
		err:   errors.New("previous block header hash mismatch"),
		label: "header_hash_mismatch",
	}
	// ErrDataHashMismatch is returned when the block data hash doesn't match the header.
	ErrDataHashMismatch = &labeledError{
		err:   errors.New("block data hash mismatch"),
		label: "data_hash_mismatch",
	}
	// ErrSignatureVerification is returned when block signature verification fails.
	ErrSignatureVerification = &labeledError{
		err:   errors.New("block signature verification failed"),
		label: "signature_verification",
	}
	// ErrConfigBlockAhead is returned when config block number is ahead of current block.
	ErrConfigBlockAhead = &labeledError{
		err:   errors.New("config block number ahead of current block"),
		label: "config_block_ahead",
	}
	// ErrLastConfigIndexMismatch is returned when block's last config index doesn't match expected.
	ErrLastConfigIndexMismatch = &labeledError{
		err:   errors.New("last config block index mismatch"),
		label: "last_config_index_mismatch",
	}
	// ErrConfigBlockSelfReference is returned when config block doesn't reference itself.
	ErrConfigBlockSelfReference = &labeledError{
		err:   errors.New("config block self-reference mismatch"),
		label: "config_block_self_reference",
	}
	// ErrGenesisBlockMismatch is returned when delivered genesis block doesn't match known genesis.
	ErrGenesisBlockMismatch = &labeledError{
		err:   errors.New("delivered genesis block mismatch"),
		label: "genesis_block_mismatch",
	}
	// ErrLastConfigIndexFetch is returned when fetching last config index from block fails.
	ErrLastConfigIndexFetch = &labeledError{
		err:   errors.New("failed to fetch last config block index"),
		label: "last_config_index_fetch",
	}
)

// labeledError wraps an error with a label for metrics categorization.
type labeledError struct {
	err   error
	label string
}

// Error implements the error interface.
func (e *labeledError) Error() string {
	return e.err.Error()
}

// Unwrap returns the underlying error for errors.Is and errors.As.
func (e *labeledError) Unwrap() error {
	return e.err
}

// Label returns the metrics label for this error.
func (e *labeledError) Label() string {
	return e.label
}

// getErrorLabel extracts a metrics-friendly label from an error.
func getErrorLabel(err error) string {
	if err == nil {
		return "success"
	}

	var le *labeledError
	if errors.As(err, &le) {
		return le.label
	}

	return "unknown_error"
}
