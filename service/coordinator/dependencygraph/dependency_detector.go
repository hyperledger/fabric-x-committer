/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

type (
	dependencyDetector struct {
		// readOnlyKeyToWaitingTxs holds a map of key to transaction that have only read the key.
		// readOnlyKeyToWaitingTxs is used to establish write-read dependency.
		readOnlyKeyToWaitingTxs keyToTransactions

		// writeOnlyKeyToWaitingTxs holds a map of key to transaction that have only written the key.
		// writeOnlyKeyToWaitingTxs is used to establish write-write and read-write dependencies.
		writeOnlyKeyToWaitingTxs keyToTransactions

		// readWriteKeyToWaitingTxs holds a map of key to transaction that have read and written the key.
		// readWriteKeyToWaitingTxs is used to establish write-read, write-write and read-write dependencies.
		readWriteKeyToWaitingTxs keyToTransactions
	}

	// keyToTransactions holds a map of key to transactions that have read or written the key.
	keyToTransactions map[string]transactionMap

	transactionMap map[*TransactionNode]any
)

func newDependencyDetector() *dependencyDetector {
	return &dependencyDetector{
		readOnlyKeyToWaitingTxs:  make(keyToTransactions),
		writeOnlyKeyToWaitingTxs: make(keyToTransactions),
		readWriteKeyToWaitingTxs: make(keyToTransactions),
	}
}

// getDependenciesOf returns a list of transactions that the given transaction depends on.
// A transaction depends on another transaction if it reads a key that the other transaction has written or
// it writes a key that the other transaction has read or written. This method returns an empty list if the
// given transaction does not depend on any other transaction. The returned list does not contain duplicates.
// This method can be called from multiple goroutines concurrently provided that addWaitingTx(), removeWaitingTx(),
// and mergeWaitingTx() are not called concurrently with this method. We can use a read-write lock but
// we are avoiding it for performance reasons and leave the synchronization to the caller.
func (d *dependencyDetector) getDependenciesOf(txNode *TransactionNode) TxNodeBatch /* dependsOn */ {
	dependsOnTxs := make(transactionMap)

	copyTxs := func(dest, src transactionMap) {
		for t := range src {
			dest[t] = nil
		}
	}

	for _, rk := range txNode.rwKeys.readsOnly {
		// read-write dependency
		copyTxs(dependsOnTxs, d.writeOnlyKeyToWaitingTxs[rk])

		// read-write dependency
		copyTxs(dependsOnTxs, d.readWriteKeyToWaitingTxs[rk])
	}

	for _, keys := range [][]string{txNode.rwKeys.writesOnly, txNode.rwKeys.readsAndWrites} {
		for _, k := range keys {
			// write-read dependency
			copyTxs(dependsOnTxs, d.readOnlyKeyToWaitingTxs[k])

			// for writeOnly, the following detects write-write dependency
			// for readWrite, the following detects read-write and write-write dependencies
			copyTxs(dependsOnTxs, d.writeOnlyKeyToWaitingTxs[k])

			// for writeOnly, the following detects write-read and write-write dependency
			// for readWrite, the following detects write-read, read-write, and write-write dependencies
			copyTxs(dependsOnTxs, d.readWriteKeyToWaitingTxs[k])
		}
	}

	depOns := make(TxNodeBatch, 0, len(dependsOnTxs))
	for depOnTx := range dependsOnTxs {
		depOns = append(depOns, depOnTx)
	}
	return depOns
}

// addWaitingTx adds the given transaction's reads and writes to the dependency detector
// so that getDependenciesOf() can consider them when calculating dependencies.
// This method is not thread-safe.
func (d *dependencyDetector) addWaitingTx(txNode *TransactionNode) {
	d.readOnlyKeyToWaitingTxs.add(txNode.rwKeys.readsOnly, txNode)
	d.writeOnlyKeyToWaitingTxs.add(txNode.rwKeys.writesOnly, txNode)
	d.readWriteKeyToWaitingTxs.add(txNode.rwKeys.readsAndWrites, txNode)
}

// mergeWaitingTx merges the waiting transaction's reads and writes from
// another dependency detector. This method is not thread-safe.
func (d *dependencyDetector) mergeWaitingTx(depDetector *dependencyDetector) {
	// NOTE: The given depDetector is not modified ever after we reach here.
	//       Hence, we don't need to copy the map and can safely assign them instead.
	d.readOnlyKeyToWaitingTxs.merge(depDetector.readOnlyKeyToWaitingTxs)
	d.writeOnlyKeyToWaitingTxs.merge(depDetector.writeOnlyKeyToWaitingTxs)
	d.readWriteKeyToWaitingTxs.merge(depDetector.readWriteKeyToWaitingTxs)
}

// removeWaitingTx removes the given transaction's reads and writes from the dependency detector
// so that getDependenciesOf() does not consider them when calculating dependencies.
// This method is not thread-safe.
func (d *dependencyDetector) removeWaitingTx(txsNode TxNodeBatch) {
	for _, txNode := range txsNode {
		d.readOnlyKeyToWaitingTxs.remove(txNode.rwKeys.readsOnly, txNode)
		d.writeOnlyKeyToWaitingTxs.remove(txNode.rwKeys.writesOnly, txNode)
		d.readWriteKeyToWaitingTxs.remove(txNode.rwKeys.readsAndWrites, txNode)
	}
}

func (keyToTx keyToTransactions) add(keys []string, tx *TransactionNode) {
	for _, key := range keys {
		txList, ok := keyToTx[key]
		if !ok {
			txList = make(map[*TransactionNode]any)
			keyToTx[key] = txList
		}
		txList[tx] = nil
	}
}

func (keyToTx keyToTransactions) remove(keys []string, tx *TransactionNode) {
	for _, k := range keys {
		switch len(keyToTx[k]) {
		case 1:
			delete(keyToTx, k)
		case 0:
			continue
		default:
			delete(keyToTx[k], tx)
		}
	}
}

func (keyToTx keyToTransactions) merge(inputKeyToTx keyToTransactions) {
	for key, srcTxList := range inputKeyToTx {
		destTxList, ok := keyToTx[key]
		if !ok {
			keyToTx[key] = srcTxList
			continue
		}
		for tx := range srcTxList {
			destTxList[tx] = nil
		}
	}
}
