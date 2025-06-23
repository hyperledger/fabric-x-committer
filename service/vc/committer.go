/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"

	"github.ibm.com/decentralized-trust-research/scalable-committer/api/protoblocktx"
	"github.ibm.com/decentralized-trust-research/scalable-committer/api/types"
	"github.ibm.com/decentralized-trust-research/scalable-committer/utils/channel"
	"github.ibm.com/decentralized-trust-research/scalable-committer/utils/monitoring/promutil"
)

// transactionCommitter is responsible for committing the transactions.
type transactionCommitter struct {
	// db is a handler for the state database holding all the committed states
	db *database
	// incomingValidatedTransactions is the channel from which the committer receives the validated transactions
	// from the validator
	incomingValidatedTransactions <-chan *validatedTransactions
	// outgoingTransactionsStatus is the channel to which the committer sends the status of the transactions
	// so that the client can be notified
	outgoingTransactionsStatus chan<- *protoblocktx.TransactionsStatus

	metrics *perfMetrics
}

// newCommitter creates a new transactionCommitter.
func newCommitter(
	db *database,
	validatedTxs <-chan *validatedTransactions,
	txsStatus chan<- *protoblocktx.TransactionsStatus,
	metrics *perfMetrics,
) *transactionCommitter {
	logger.Info("Initializing new committer")
	return &transactionCommitter{
		db:                            db,
		incomingValidatedTransactions: validatedTxs,
		outgoingTransactionsStatus:    txsStatus,
		metrics:                       metrics,
	}
}

func (c *transactionCommitter) run(ctx context.Context, numWorkers int) error {
	logger.Infof("Starting transactionCommitter with %d workers", numWorkers)
	g, eCtx := errgroup.WithContext(ctx)
	for range numWorkers {
		g.Go(func() error {
			return c.commit(eCtx)
		})
	}

	return g.Wait()
}

func (c *transactionCommitter) commit(ctx context.Context) error {
	// NOTE: Three retry is adequate for now. We can make it configurable in the future.
	var txsStatus *protoblocktx.TransactionsStatus
	var err error

	incomingValidatedTransactions := channel.NewReader(ctx, c.incomingValidatedTransactions)
	outgoingTransactionsStatus := channel.NewWriter(ctx, c.outgoingTransactionsStatus)
	for {
		vTx, ok := incomingValidatedTransactions.Read()
		if !ok {
			return nil
		}

		logger.Debugf("Batch of %d new writes, %d new non blind writes and %d blind writes "+
			"validated TXs in the committer.",
			len(vTx.newWrites), len(vTx.validTxNonBlindWrites), len(vTx.validTxBlindWrites))
		start := time.Now()
		// There are certain errors for which we need to retry the commit operation.
		// Refer to YugabyteDB documentation for retryable error.
		// Rather than distinguishing retryable transaction error, we retry for all errors.
		// This is for simplicity and we can improve it in future.
		// TODO: Add test to ensure commit is retried.
		txsStatus, err = c.commitTransactions(ctx, vTx)
		if err != nil {
			return fmt.Errorf("failed to commit transactions: %w", err)
		}

		promutil.Observe(c.metrics.committerTxBatchLatencySeconds, time.Since(start))
		outgoingTransactionsStatus.Write(txsStatus)
		logger.Debugf("Batch of %d new writes, %d new non blind writes and %d blind writes "+
			"sent from the committer to the output.",
			len(vTx.newWrites), len(vTx.validTxNonBlindWrites), len(vTx.validTxBlindWrites))
	}
}

func (c *transactionCommitter) commitTransactions(
	ctx context.Context,
	vTx *validatedTransactions,
) (*protoblocktx.TransactionsStatus, error) {
	// We eliminate blind writes outside the retry loop to avoid doing it more than once.
	if err := c.populateVersionsAndCategorizeBlindWrites(ctx, vTx); err != nil {
		return nil, err
	}

	// Ideally, we should add BlindWrite entries with a null version to the
	// `readToTxIDs` map. This is because transactions can be resubmitted to multiple
	// `vcservice` instances during connection issue between the coordinator and
	// a vcservice instance. One instance might successfully write the BlindWrite,
	// while others encounter a conflict at commit time because the new keys
	// already exist. In these cases, we need to mark the TxID as invalid.
	// Since conflicts are processed against `readToTxIDs`,
	// we must include BlindWrites with a null version in this map.
	//
	// Eventually, the transaction will receive the correct committed status based on
	// TxID deduplication.
	//
	// However, we can avoid this complexity if we commit transaction IDs to the
	// `tx_status` table *before* processing the writes. This ensures that blind writes
	// will not return a conflict, as we would reject the transaction earlier due
	// to a duplicate transaction ID.

	// Theoretically, we can only retry twice. Once for attempting to insert existing keys, and once for attempting
	// to reuse transaction IDs.
	// However, we still limit the number of retries to some arbitrary number to avoid an endless loop due to a bug.
	maxRetriesToRemoveAllInvalidTxs := 1024
	for range maxRetriesToRemoveAllInvalidTxs {
		// Group the writes by namespace so that we can commit to each table independently.
		info := &statesToBeCommitted{
			updateWrites: groupWritesByNamespace(vTx.validTxNonBlindWrites),
			newWrites:    groupWritesByNamespace(vTx.newWrites),
			batchStatus:  prepareStatusForCommit(vTx),
			txIDToHeight: vTx.txIDToHeight,
		}

		var (
			conflicts  namespaceToReads
			duplicates []TxID
			err        error
		)
		if retryErr := c.db.retry.Execute(ctx, func() error {
			conflicts, duplicates, err = c.db.commit(ctx, info)
			return err
		}); retryErr != nil {
			return nil, err
		}

		if conflicts.empty() && len(duplicates) == 0 {
			// NOTE: If a submitted transaction is invalid for multiple reasons, including a duplicate
			//       transaction ID, the committer prioritizes Status_ABORTED_DUPLICATE_TXID over any other
			//       invalid status code. Even if a previously committed transaction is resubmitted (regardless
			//       of its original status), the committer will always return Status_ABORTED_DUPLICATE_TXID.
			//       The setCorrectStatusForDuplicateTxID function checks whether the transaction is truly a
			//       duplicate or a resubmission. If it is a resubmission, it retrieves the correct status from
			//       the tx_status table.
			if err := c.setCorrectStatusForDuplicateTxID(ctx, info.batchStatus, info.txIDToHeight); err != nil {
				return nil, fmt.Errorf("failed to set correct status for duplicate txs: %w", err)
			}
			return info.batchStatus, nil
		}

		if err := vTx.invalidateTxsOnReadConflicts(conflicts); err != nil {
			return nil, fmt.Errorf("failed to invalidate transactions on read conflicts: %w", err)
		}
		vTx.updateInvalidTxs(duplicates, protoblocktx.Status_ABORTED_DUPLICATE_TXID)
	}

	return nil, errors.Newf("[BUG] commit failed after %d retries", maxRetriesToRemoveAllInvalidTxs)
}

// prepareStatusForCommit construct transaction status.
func prepareStatusForCommit(vTx *validatedTransactions) *protoblocktx.TransactionsStatus {
	txCommitStatus := &protoblocktx.TransactionsStatus{
		Status: map[string]*protoblocktx.StatusWithHeight{},
	}

	setStatus := func(txID TxID, status protoblocktx.Status) {
		h := vTx.txIDToHeight[txID]
		txCommitStatus.Status[string(txID)] = types.CreateStatusWithHeight(status, h.BlockNum, int(h.TxNum))
	}

	for txID, status := range vTx.invalidTxStatus {
		setStatus(txID, status)
	}

	for _, lst := range []transactionToWrites{
		vTx.validTxNonBlindWrites, vTx.validTxBlindWrites, vTx.newWrites,
	} {
		for txID := range lst {
			setStatus(txID, protoblocktx.Status_COMMITTED)
		}
	}

	return txCommitStatus
}

// populateVersionsAndCategorizeBlindWrites fetches the current version of the blind-writes keys, and assigns them
// to the appropriate category (new/update).
func (c *transactionCommitter) populateVersionsAndCategorizeBlindWrites(
	ctx context.Context, vTx *validatedTransactions,
) error {
	state := make(map[string]keyToVersion)
	for nsID, writes := range groupWritesByNamespace(vTx.validTxBlindWrites) {
		// TODO: Though we could run the following in a goroutine per namespace, we restrain
		// 		 from doing so till we evaluate the performance
		versionOfPresentKeys, err := c.db.queryVersionsIfPresent(ctx, nsID, writes.keys)
		if err != nil {
			return err
		}
		state[nsID] = versionOfPresentKeys
	}

	// Place blind writes to the appropriate map (new/update)
	for curTxID, txWrites := range vTx.validTxBlindWrites {
		for ns, nsWrites := range txWrites {
			nsState := state[ns]
			for i, key := range nsWrites.keys {
				if ver, present := nsState[string(key)]; present {
					nextVer := ver + 1
					vTx.validTxNonBlindWrites.getOrCreate(curTxID, ns).append(key, nsWrites.values[i], nextVer)
				} else {
					vTx.newWrites.getOrCreate(curTxID, ns).append(key, nsWrites.values[i], 0)
				}
			}
		}
	}

	// Clear the blind writes map
	vTx.validTxBlindWrites = make(transactionToWrites)

	return nil
}

func (c *transactionCommitter) setCorrectStatusForDuplicateTxID(
	ctx context.Context,
	txsStatus *protoblocktx.TransactionsStatus,
	txIDToHeight transactionIDToHeight,
) error {
	var dupTxIDs [][]byte
	for id, s := range txsStatus.Status {
		if s.Code == protoblocktx.Status_ABORTED_DUPLICATE_TXID {
			dupTxIDs = append(dupTxIDs, []byte(id))
		}
	}

	if len(dupTxIDs) == 0 {
		return nil
	}

	idStatusHeight, err := c.db.readStatusWithHeight(ctx, dupTxIDs)
	if err != nil {
		return err
	}

	if idStatusHeight == nil {
		return nil
	}

	for txID, sWithHeight := range idStatusHeight {
		if types.AreSame(txIDToHeight[TxID(txID)], types.NewHeight(sWithHeight.BlockNumber, sWithHeight.TxNumber)) {
			txsStatus.Status[txID] = sWithHeight
		}
	}

	return nil
}

func groupWritesByNamespace(txWrites transactionToWrites) namespaceToWrites {
	nsToWrites := make(namespaceToWrites)

	for _, nsWrites := range txWrites {
		for ns, writes := range nsWrites {
			if writes.empty() {
				continue
			}
			curNsWrites := nsToWrites.getOrCreate(ns)
			curNsWrites.appendWrites(writes)
		}
	}

	return nsToWrites
}
