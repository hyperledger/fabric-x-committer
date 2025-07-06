/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

// transactionValidator validates the reads of transactions against the committed states.
type transactionValidator struct {
	// db is the state database holding all the committed states.
	db *database
	// incomingPreparedTransactions is the channel from which the validator receives prepared transactions.
	incomingPreparedTransactions <-chan *preparedTransactions
	// outgoingValidatedTransactions is the channel to which the validator sends validated transactions so that
	// the committer can commit them.
	outgoingValidatedTransactions chan<- *validatedTransactions

	metrics *perfMetrics
}

// validatedTransactions contains the writes of valid transactions and the txIDs of invalid transactions.
type validatedTransactions struct {
	validTxNonBlindWrites transactionToWrites
	validTxBlindWrites    transactionToWrites
	newWrites             transactionToWrites
	readToTxIDs           readToTransactions
	invalidTxStatus       map[TxID]protoblocktx.Status
	txIDToHeight          transactionIDToHeight
}

func (v *validatedTransactions) Debug() {
	if logger.Level() > zapcore.DebugLevel {
		return
	}
	logger.Debugf("total validated: %d\n\t"+
		"valid non-blind writes: %d\n\t"+
		"valid blind writes: %d\n\t"+
		"new writes: %d\n\treads: %d\n\tinvalid: %d\n",
		len(v.validTxNonBlindWrites)+len(v.validTxBlindWrites)+len(v.newWrites)+
			len(v.readToTxIDs)+len(v.invalidTxStatus),
		len(v.validTxNonBlindWrites), len(v.validTxBlindWrites), len(v.newWrites),
		len(v.readToTxIDs), len(v.invalidTxStatus))
}

// newValidator creates a new validator.
func newValidator(
	db *database,
	preparedTxs <-chan *preparedTransactions,
	validatedTxs chan<- *validatedTransactions,
	metrics *perfMetrics,
) *transactionValidator {
	logger.Info("Initializing new validator")
	return &transactionValidator{
		db:                            db,
		incomingPreparedTransactions:  preparedTxs,
		outgoingValidatedTransactions: validatedTxs,
		metrics:                       metrics,
	}
}

// start starts the validator with the given number of workers.
func (v *transactionValidator) run(ctx context.Context, numWorkers int) error {
	logger.Infof("Starting Validator with %d workers", numWorkers)
	g, eCtx := errgroup.WithContext(ctx)
	for range numWorkers {
		g.Go(func() error {
			return v.validate(eCtx)
		})
	}

	return g.Wait()
}

func (v *transactionValidator) validate(ctx context.Context) error {
	incomingPreparedTransactions := channel.NewReader(ctx, v.incomingPreparedTransactions)
	outgoingValidatedTransactions := channel.NewWriter(ctx, v.outgoingValidatedTransactions)

	for {
		prepTx, ok := incomingPreparedTransactions.Read()
		if !ok {
			return nil
		}

		logger.Debug("Batch of prepared TXs in the validator.")
		prepTx.Debug()
		start := time.Now()
		// Step 1: we validate reads and collect conflicting reads per namespace.
		// TODO: We can run per namespace validation in parallel. However, we should not
		// 		 over parallelize and make contention among preparer, validator, and committer
		//       goroutines to acquire the CPU. Based on performance evaluation, we can decide
		//       to run per namespace validation in parallel.
		nsToReadConflicts, valErr := v.validateReads(ctx, prepTx.nsToReads)
		if valErr != nil {
			return valErr
		}
		// Step 2: we construct validated transactions by removing the writes of invalid transactions
		// and recording the txIDs of invalid transactions.
		vTxs := &validatedTransactions{
			validTxNonBlindWrites: prepTx.txIDToNsNonBlindWrites,
			validTxBlindWrites:    prepTx.txIDToNsBlindWrites,
			newWrites:             prepTx.txIDToNsNewWrites,
			readToTxIDs:           prepTx.readToTxIDs,
			invalidTxStatus:       prepTx.invalidTxIDStatus,
			txIDToHeight:          prepTx.txIDToHeight,
		}
		if err := vTxs.invalidateTxsOnReadConflicts(nsToReadConflicts); err != nil {
			return err
		}

		promutil.Observe(v.metrics.validatorTxBatchLatencySeconds, time.Since(start))
		outgoingValidatedTransactions.Write(vTxs)

		logger.Debugf("Validator sent batch validated TXs to the committer (%d non blide writes and %d blind writes)",
			len(vTxs.validTxNonBlindWrites), len(vTxs.validTxBlindWrites))
		vTxs.Debug()
	}
}

func (v *transactionValidator) validateReads(
	ctx context.Context,
	nsToReads namespaceToReads,
) (namespaceToReads /* conflicts */, error) {
	// nsToReadConflicts maintains all conflicting reads per namespace.
	// nsID -> readConflicts{keys, versions}.
	nsToReadConflicts := make(namespaceToReads)

	for nsID, r := range nsToReads {
		conflicts, err := v.db.validateNamespaceReads(ctx, nsID, r)
		if err != nil {
			return nil, err
		}

		readConflicts := nsToReadConflicts.getOrCreate(nsID)
		readConflicts.appendMany(conflicts.keys, conflicts.versions)
	}

	return nsToReadConflicts, nil
}

func (p *preparedTransactions) Debug() {
	if logger.Level() > zapcore.DebugLevel {
		return
	}
	logger.Debugf("total prepared: %d\n\tvalid non-blind writes: %d\n\t"+
		"valid blind writes: %d\n\tnew writes: %d\n\treads: %d\n",
		len(p.txIDToNsNonBlindWrites)+len(p.txIDToNsBlindWrites)+
			len(p.txIDToNsNewWrites)+len(p.readToTxIDs),
		len(p.txIDToNsNonBlindWrites), len(p.txIDToNsBlindWrites),
		len(p.txIDToNsNewWrites), len(p.readToTxIDs))
}

func (v *validatedTransactions) invalidateTxsOnReadConflicts(nsToReadConflicts namespaceToReads) error {
	// For each conflicting read, we find the transactions which made the
	// read and mark them as invalid. Further, the writes of those invalid
	// transactions are removed from the valid writes.
	for nsID, readConflicts := range nsToReadConflicts {
		for index, key := range readConflicts.keys {
			cr := newCmpRead(nsID, key, readConflicts.versions[index])
			txIDs, ok := v.readToTxIDs[cr]
			if !ok {
				return errors.Newf("read %v not found in readToTransactionIndices", cr)
			}

			v.updateInvalidTxs(txIDs, protoblocktx.Status_ABORTED_MVCC_CONFLICT)
		}
	}

	return nil
}

func (v *validatedTransactions) updateInvalidTxs(txIDs []TxID, status protoblocktx.Status) {
	for _, tID := range txIDs {
		delete(v.validTxNonBlindWrites, tID)
		delete(v.validTxBlindWrites, tID)
		delete(v.newWrites, tID)
		v.invalidTxStatus[tID] = status
	}
}
