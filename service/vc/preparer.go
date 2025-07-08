/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protovcservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

type (
	// transactionPreparer prepares transaction batches for validation and commit.
	transactionPreparer struct {
		// incomingTransactionBatch is an input to the preparer
		incomingTransactionBatch <-chan *protovcservice.TransactionBatch
		// outgoingPreparedTransactions is an output of the preparer and an input to the validator
		outgoingPreparedTransactions chan<- *preparedTransactions
		// metrics is the metrics collector
		metrics *perfMetrics
	}

	// TxID is the type defining the transaction identifier.
	TxID string

	// preparedTransactions is a list of transactions that are prepared for validation and commit
	// preparedTransactions is NOT thread safe.
	preparedTransactions struct {
		// read validation fields:
		nsToReads   namespaceToReads   // Maps namespaces to reads performed within them.
		readToTxIDs readToTransactions // Maps reads to transaction IDs that executed them.

		// nsToReads is used to verify the reads performed by each transaction in each namespace.
		// If a read is found to be invalid due to version mismatch, transactions which performed
		// this invalid read would be marked invalid and all writes of these transactions present
		// in the three categories of writes would be removed as well.
		// write categorization fields:
		txIDToNsNonBlindWrites transactionToWrites // Maps txIDs to non-blind writes per namespace.
		txIDToNsBlindWrites    transactionToWrites // Maps txIDs to blind writes per namespace.
		txIDToNsNewWrites      transactionToWrites // Maps txIDs to new writes per namespace.

		invalidTxIDStatus map[TxID]protoblocktx.Status // Maps txIDs to the status.
		txIDToHeight      transactionIDToHeight        // Maps txIDs to height in the blockchain.
	}

	transactionIDToHeight map[TxID]*types.Height

	// namespaceToReads maps a namespace ID to a list of reads performed within that namespace.
	namespaceToReads map[string]*reads

	// reads represents a list of keys and their corresponding versions.
	reads struct {
		keys     [][]byte
		versions []*uint64
	}

	// readToTransactions maps a read to the transaction ID.
	// It is used to find the txID of invalid transactions when a read is invalid
	// i.e., the read version is not matching the committed version.
	readToTransactions map[comparableRead][]TxID

	// comparableRead defines a read with fields suitable for use as a map key (for uniqueness).
	// We use int64 instead of uint64 for version to allow encoding nil version as -1.
	comparableRead struct {
		nsID    string
		key     string
		version int64
	}

	transactionToWrites map[TxID]namespaceToWrites

	namespaceToWrites map[string]*namespaceWrites

	namespaceWrites struct {
		keys     [][]byte
		values   [][]byte
		versions []uint64
	}
)

// newPreparer creates a new preparer instance with input channel txBatch and output channel preparedTxs.
func newPreparer(
	txBatch <-chan *protovcservice.TransactionBatch,
	preparedTxs chan<- *preparedTransactions,
	metrics *perfMetrics,
) *transactionPreparer {
	logger.Info("Initializing new preparer")
	return &transactionPreparer{
		incomingTransactionBatch:     txBatch,
		outgoingPreparedTransactions: preparedTxs,
		metrics:                      metrics,
	}
}

func newCmpRead(nsID string, key []byte, version *uint64) comparableRead {
	var ver int64 = -1
	if version != nil {
		ver = int64(*version) //nolint:gosec // convert to int64 to allow negative value.
	}
	return comparableRead{nsID, string(key), ver}
}

func (p *transactionPreparer) run(ctx context.Context, numWorkers int) error {
	logger.Infof("Starting transaction preparer with %d workers", numWorkers)
	g, eCtx := errgroup.WithContext(ctx)

	for range numWorkers {
		g.Go(func() error {
			p.prepare(eCtx)
			return nil
		})
	}

	return g.Wait()
}

// prepare reads transactions from the txBatch channel and prepares them for validation.
// It groups reads by namespace and creates a map of reads to transactions to find
// the index of invalid transactions when a read is invalid.
// It sends the prepared transactions to the preparedTxs channel which is an input
// to the validator.
func (p *transactionPreparer) prepare(ctx context.Context) { //nolint:gocognit
	incomingTransactionBatch := channel.NewReader(ctx, p.incomingTransactionBatch)
	outgoingPreparedTransactions := channel.NewWriter(ctx, p.outgoingPreparedTransactions)
	for {
		txBatch, ok := incomingTransactionBatch.Read()
		if !ok {
			return
		}
		logger.Debugf("New batch with %d in the preparer.", len(txBatch.Transactions))
		start := time.Now()
		prepTxs := &preparedTransactions{
			nsToReads:              make(namespaceToReads),
			readToTxIDs:            make(readToTransactions),
			txIDToNsNonBlindWrites: make(transactionToWrites),
			txIDToNsBlindWrites:    make(transactionToWrites),
			txIDToNsNewWrites:      make(transactionToWrites),
			invalidTxIDStatus:      make(map[TxID]protoblocktx.Status),
			txIDToHeight:           make(transactionIDToHeight),
		}

		for _, tx := range txBatch.Transactions {
			if _, ok := prepTxs.txIDToHeight[TxID(tx.ID)]; ok {
				// It's possible to receive the same transaction multiple times within a batch
				// if there's a connection issue between the coordinator and the vcservice.
				// Although the coordinator does not send the same transaction more than once
				// in a single batch, connection failures may cause the same transaction to
				// appear in multiple batches. Because the vcservice's minBatchSize limit
				// can merge these batches, the same transaction may appear multiple times
				// within one batch.
				// Hence, we detect such duplicate transactions and ignore them.
				continue
			}
			prepTxs.txIDToHeight[TxID(tx.ID)] = &types.Height{BlockNum: tx.BlockNumber, TxNum: tx.TxNum}
			// If the preliminary invalid transaction status is set,
			// the vcservice does not need to validate the transaction,
			// but it will still commit the status only if the txID is not a duplicate.
			if tx.PrelimInvalidTxStatus != nil {
				logger.Debugf("Transaction %s marked as preliminarily invalid with status: %v",
					tx.ID, tx.PrelimInvalidTxStatus.Code)
				prepTxs.invalidTxIDStatus[TxID(tx.ID)] = tx.PrelimInvalidTxStatus.Code
				continue
			}

			for _, nsOperations := range tx.Namespaces {
				tID := TxID(tx.ID)
				logger.Debugf("Preparing namespace %s in transaction with ID %s ", nsOperations.NsId, tx.ID)
				prepTxs.addReadsOnly(tID, nsOperations)
				prepTxs.addReadWrites(tID, nsOperations)
				prepTxs.addBlindWrites(tID, nsOperations)

				// each transaction has a namespaceID and a version
				// on which the transaction was executed. We need to
				// ensure that the version is not stale. If it is,
				// we need to reject the transaction. This is done by
				// adding the namespaceID, and version to the reads-only
				// list of the metaNamespaceID.
				switch nsOperations.NsId {
				case types.MetaNamespaceID:
					// Meta TX is dependent on the config TX.
					prepTxs.addReadsOnly(tID, &protoblocktx.TxNamespace{
						NsId: types.ConfigNamespaceID,
						ReadsOnly: []*protoblocktx.Read{{
							Key:     []byte(types.ConfigKey),
							Version: &nsOperations.NsVersion,
						}},
					})
				case types.ConfigNamespaceID:
					// A config TX is independent.
				default:
					prepTxs.addReadsOnly(tID, &protoblocktx.TxNamespace{
						NsId: types.MetaNamespaceID,
						ReadsOnly: []*protoblocktx.Read{{
							Key:     []byte(nsOperations.NsId),
							Version: &nsOperations.NsVersion,
						}},
					})
				}
			}
		}

		for ns, read := range prepTxs.nsToReads {
			if len(read.keys) == 0 {
				delete(prepTxs.nsToReads, ns)
			}
		}

		for _, lst := range []transactionToWrites{
			prepTxs.txIDToNsNonBlindWrites, prepTxs.txIDToNsBlindWrites, prepTxs.txIDToNsNewWrites,
		} {
			lst.clearEmpty()
		}

		promutil.Observe(p.metrics.preparerTxBatchLatencySeconds, time.Since(start))
		outgoingPreparedTransactions.Write(prepTxs)

		logger.Debug("Transaction preparing finished.")
	}
}

// addReadsOnly adds reads-only to the prepared transactions.
func (p *preparedTransactions) addReadsOnly(id TxID, ns *protoblocktx.TxNamespace) {
	if len(ns.ReadsOnly) == 0 {
		logger.Debugf("No read-only entries found in namespace %s", ns.NsId)
		return
	}

	logger.Debugf("Adding %d read-only entries found in namespace %s to the prepared transaction",
		len(ns.ReadsOnly), ns.NsId)
	nsReads := p.nsToReads.getOrCreate(ns.NsId)

	for _, r := range ns.ReadsOnly {
		// When more than one txs read the same key, we only need to add the key once for validation.
		// If the read is already present in the list, we can skip adding it.
		cr := newCmpRead(ns.NsId, r.Key, r.Version)
		v, present := p.readToTxIDs[cr]
		p.readToTxIDs[cr] = append(v, id)
		if !present {
			nsReads.append(r.Key, r.Version)
		}
	}
}

// addReadWrites adds read-writes to the prepared transactions.
func (p *preparedTransactions) addReadWrites(id TxID, ns *protoblocktx.TxNamespace) {
	if len(ns.ReadWrites) == 0 {
		logger.Debugf("No read-write entries found in namespace %s", ns.NsId)
		return
	}

	logger.Debugf("Adding %d read-write entries found in namespace %s to the prepared transaction",
		len(ns.ReadWrites), ns.NsId)
	nsReads := p.nsToReads.getOrCreate(ns.NsId)
	nsWrites := p.txIDToNsNonBlindWrites.getOrCreate(id, ns.NsId)
	newWrites := p.txIDToNsNewWrites.getOrCreate(id, ns.NsId)

	for _, rw := range ns.ReadWrites {
		// In read-writes, duplicates are not possible between transactions. This is because
		// read-write and write-write dependency ensures that only one of the transactions is
		// chosen for the validation and commit.
		cr := newCmpRead(ns.NsId, rw.Key, rw.Version)
		rtt, present := p.readToTxIDs[cr]
		p.readToTxIDs[cr] = append(rtt, id)

		if rw.Version != nil {
			if !present {
				nsReads.append(rw.Key, rw.Version)
			}
			nsWrites.append(rw.Key, rw.Value, *rw.Version+1)
		} else {
			// This version value will not be used because we do not assign the version
			// when inserting a new key. We use the DB default value instead, which is 0.
			newWrites.append(rw.Key, rw.Value, 0)
		}
	}
}

// addBlindWrites adds the blind writes to the prepared transactions.
func (p *preparedTransactions) addBlindWrites(id TxID, ns *protoblocktx.TxNamespace) {
	if len(ns.BlindWrites) == 0 {
		logger.Debugf("No blind writes entries found in namespace %s", ns.NsId)
		return
	}

	logger.Debugf("Adding %d bline writes entries found in namespace %s to the prepared transaction",
		len(ns.BlindWrites), ns.NsId)
	nsWrites := p.txIDToNsBlindWrites.getOrCreate(id, ns.NsId)

	for _, w := range ns.BlindWrites {
		nsWrites.append(w.Key, w.Value, 0)
	}
}

func (nw namespaceToWrites) empty() bool {
	if nw == nil {
		return true
	}

	for _, writes := range nw {
		if !writes.empty() {
			return false
		}
	}

	return true
}

func (nw namespaceToWrites) getOrCreate(nsID string) *namespaceWrites {
	nsWrites, ok := nw[nsID]
	if !ok {
		nsWrites = &namespaceWrites{}
		nw[nsID] = nsWrites
	}
	return nsWrites
}

func (nr namespaceToReads) getOrCreate(nsID string) *reads {
	nsRead, ok := nr[nsID]
	if !ok {
		nsRead = &reads{}
		nr[nsID] = nsRead
	}
	return nsRead
}

func (tw transactionToWrites) getOrCreate(id TxID, nsID string) *namespaceWrites {
	nsToWrites, ok := tw[id]
	if !ok {
		nsToWrites = make(namespaceToWrites)
		tw[id] = nsToWrites
	}

	nsWrites, ok := nsToWrites[nsID]
	if !ok {
		nsWrites = &namespaceWrites{}
		nsToWrites[nsID] = nsWrites
	}

	return nsWrites
}

func (tw transactionToWrites) clearEmpty() {
	var emptyIDs []TxID
	for id, ntw := range tw {
		if ntw.empty() {
			emptyIDs = append(emptyIDs, id)
			continue
		}

		var emptyNsID []string
		for nsID, nw := range ntw {
			if nw.empty() {
				emptyNsID = append(emptyNsID, nsID)
			}
		}

		for _, nsID := range emptyNsID {
			delete(ntw, nsID)
		}
	}

	for _, id := range emptyIDs {
		delete(tw, id)
	}
}

func (r *reads) append(key []byte, version *uint64) {
	r.keys = append(r.keys, key)
	r.versions = append(r.versions, version)
}

func (r *reads) appendMany(keys [][]byte, versions []*uint64) {
	r.keys = append(r.keys, keys...)
	r.versions = append(r.versions, versions...)
}

func (nw *namespaceWrites) append(key, value []byte, version uint64) {
	nw.keys = append(nw.keys, key)
	nw.values = append(nw.values, value)
	nw.versions = append(nw.versions, version)
}

func (nw *namespaceWrites) appendWrites(other *namespaceWrites) {
	nw.keys = append(nw.keys, other.keys...)
	nw.values = append(nw.values, other.values...)
	nw.versions = append(nw.versions, other.versions...)
}

func (nw *namespaceWrites) empty() bool {
	return nw == nil || len(nw.keys) == 0
}

func (r *reads) empty() bool {
	return r == nil || len(r.keys) == 0
}

func (nr namespaceToReads) empty() bool {
	if nr == nil {
		return true
	}

	for _, r := range nr {
		if !r.empty() {
			return false
		}
	}

	return true
}
