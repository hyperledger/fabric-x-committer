<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Validator-Committer Service

1.  [Overview](#1-overview)
2.  [Core Responsibilities](#2-core-responsibilities)
3.  [Configuration](#3-configuration)
4.  [Database Schema](#4-database-schema)
5.  [Workflow Details](#5-workflow-details)
    * [Task 1. Preparing Transaction Batches](#task-1-preparing-transaction-batches)
    * [Task 2. Validating Transaction Read-Sets](#task-2-validating-transaction-read-sets)
    * [Task 3. Committing Valid Transactions to the Database](#task-3-committing-valid-transactions-to-the-database)
6.  [gRPC Service API](#6-grpc-service-api)
7.  [Failure and Recovery](#7-failure-and-recovery)

## 1. Overview

The [Validator-Committer (VC) service](https://github.com/hyperledger/fabric-x-committer/tree/main/service/vc) is a core component responsible for the final stages of transaction processing.
It operates downstream from the Coordinator, receiving batches of transactions that are internally conflict-free. 
This means that these conflict-free transactions do not conflict with any other active transactions being processed concurrently.
Its primary function is to perform optimistic concurrency control by validating each transaction's read-set against the current state in
the database. Transactions that pass this validation are then committed, and their write-sets are applied to the database.

## 2. Core Responsibilities

The VC service performs three main tasks:

1.  **Receive Transactions:** Receives batches of conflict-free transactions from the Coordinator via a gRPC stream.
2.  **Validate and Commit:** Performs Multi-Version Concurrency Control (MVCC) checks on transaction read-sets against the committed
database state. It commits valid transactions and reports the final status (e.g., committed, aborted due to MVCC conflict)
of every transaction back to the Coordinator.
3.  **Provide Data Access:** Exposes gRPC APIs that allow the Coordinator to read from and
write to the state database, ensuring consistent data access.

## 3. Configuration

The VC service requires the following configuration settings, provided in a YAML [configuration file](https://github.com/hyperledger/fabric-x-committer/blob/main/cmd/config/samples/vcservice.yaml):

* *Database Endpoints*: Address(es) of the state database nodes that the service will connect to.
* *Listen Address*: The local address and port (e.g., 0.0.0.0:7056) where the VC service will host its gRPC service, enabling
the Coordinator to connect and send transaction batches.

## 4. Database Schema

The VC service is responsible for managing several tables within the state database. These tables store the transaction statuses,
world state for each namespace, and internal metadata. The tables are categorized into system tables and user namespace tables.

### a. System Tables

* **Transaction Status Table (`tx_status`)**: This table stores the final status of every transaction processed by the system.

| Column Name | Data Type | Description/Constraints                                                                                            |
| :---------- | :-------- | :----------------------------------------------------------------------------------------------------------------  |
| `tx_id`     | `BYTEA`   | The transaction ID. Primary Key.                                                                                   |
| `status`    | `INTEGER` | The final status code.                                                                                             |
| `height`    | `BYTEA`   | A combination of the block number and the transaction's index within the block, encoded as order-preserving bytes. |

* **Namespace Metadata Table (`ns__meta`)**: This table stores metadata about each namespace, primarily its endorsement policy.

| Column Name | Data Type | Description/Constraints                            |
| :---------- | :-------- | :------------------------------------------------- |
| `key`       | `BYTEA`   | The namespace ID. Primary Key.                     |
| `value`     | `BYTEA`   | The endorsement policy for the namespace.          |
| `version`   | `BIGINT`  | The version of the key-value pair for MVCC checks. |

* **Configuration Table (`ns__config`)**: This table stores the system's configuration transaction. It holds a single entry.

| Column Name | Data Type | Description/Constraints                            |
| :---------- | :-------- | :------------------------------------------------- |
| `key`       | `BYTEA`   | A fixed key, `_config`. Primary Key.               |
| `value`     | `BYTEA`   | The system configuration transaction.              |
| `version`   | `BIGINT`  | Not Applicable                                     |

* **Metadata Table (`metadata`)**: This table is a simple key-value store for other internal system metadata. For now, we
only store the last committed block number.

| Column Name | Data Type | Description/Constraints        |
| :---------- | :-------- | :----------------------------- |
| `key`       | `BYTEA`   | The metadata key. Primary Key. |
| `value`     | `BYTEA`   | The metadata value.            |

### b. User Namespace Tables

A separate table is created for each user-defined namespace to store its world state.

| Column Name | Data Type | Description/Constraints                                |
| :---------- | :-------- | :----------------------------------------------------- |
| `key`       | `BYTEA`   | The key for a state entry. Primary Key.                |
| `value`     | `BYTEA`   | The value associated with the key.                     |
| `version`   | `BIGINT`  | The version of the key-value pair for MVCC checks.     |

### c. Stored Procedures

To optimize performance and minimize network round-trips, the VC service relies heavily on stored procedures executed within the database.

| Procedure Name                      | Description                                                                                                                                                             |
| :---------------------------------- | :---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `insert_tx_status`                  | Stores transaction statuses in bulk. On a primary key violation (duplicate `tx_id`), it returns the violating `tx_id`.                                                    |
| `validate_reads_ns_${NAMESPACE_ID}` | Performs MVCC validation for a batch of reads within a specific namespace. It returns the indices of keys whose committed version differs from the passed version.         |
| `update_ns_${NAMESPACE_ID}`         | Updates existing keys within a namespace with new values and versions.                                                                                                  |
| `insert_ns_${NAMESPACE_ID}`         | Inserts new key-value pairs into a namespace. If a key already exists (violating the primary key constraint), it returns the keys that caused the violation.                 |

Note: `${NAMESPACE_ID}` is a placeholder that is replaced with the actual namespace ID at runtime to invoke the correct procedure for a given data partition.

## 5. Workflow Details

The VC service runs three long-running, pipelined tasks:

* **Task 1**: Prepares incoming transaction batches for validation.
* **Task 2**: Validates the read-sets of transactions against the committed database state.
* **Task 3**: Commits the write-sets of valid transactions to the database.

Communication among these tasks is managed by a series of internal channels:

1.  `toPrepareTxs`: An input channel carrying raw transaction batches from the Coordinator to Task 1.
2.  `preparedTxs`: Carries prepared transaction data from Task 1 to Task 2.
3.  `validatedTxs`: Carries validated transaction data from Task 2 to Task 3.
4.  `txsStatus`: An output channel where Task 3 places the final status of all processed transactions to be sent back to the Coordinator.

### Task 1. Preparing Transaction Batches

This task consumes transaction batches from the `toPrepareTxs` channel and transforms them into a structured format optimized for validation.
The primary goal is to organize the reads and writes from all transactions in the batch for efficient processing in the subsequent stages.

**a. Dequeueing Transaction Batches:** The [preparer](https://github.com/hyperledger/fabric-x-committer/blob/main/service/vc/preparer.go) reads a batch of transactions from the `toPrepareTxs` input queue.

**b. Data Structuring:** It iterates through each transaction in the batch and populates a `preparedTransactions` struct. This involves:

* Mapping all transaction reads to their respective namespaces.
* Creating a reverse map from each specific read (key-version pair) back to the transaction IDs that performed it. This is crucial for quickly
identifying all invalid transactions if a single read proves invalid.
* Categorizing all transaction writes into new writes, blind writes, and non-blind writes.

The main data structure produced by this task is `preparedTransactions`:

```go
// preparedTransactions contains all the necessary information from a batch,
// structured for efficient validation. It is NOT thread-safe.
type preparedTransactions struct {
    // Read validation fields
    nsToReads   namespaceToReads   // Maps namespaces to all reads within them.
    readToTxIDs readToTransactions // Maps a specific read to the txIDs that performed it.

    // Write categorization fields
    txIDToNsNonBlindWrites transactionToWrites // Maps txIDs to their non-blind writes.
    txIDToNsBlindWrites    transactionToWrites // Maps txIDs to their blind writes.
    txIDToNsNewWrites      transactionToWrites // Maps txIDs to their new writes.

    // Transaction metadata
    invalidTxIDStatus map[TxID]protoblocktx.Status // Stores status for pre-invalidated txs.
    txIDToHeight      transactionIDToHeight      // Maps txIDs to their blockchain height.
}

// readToTransactions maps a read to the transaction IDs that performed it.
// Used to find all transactions that become invalid when a read version
// does not match the committed version.
type readToTransactions map[comparableRead][]TxID

// transactionToWrites maps a transaction ID to its writes, organized by namespace.
type transactionToWrites map[TxID]namespaceToWrites

// namespaceToWrites maps a namespace to all writes within it.
type namespaceToWrites map[string]*writes

type writes struct {
    keys     [][]byte
    values   [][]byte
    versions [][]byte
}

```

**c. Rationale for Write Categorization:**
The categorization of writes is a key performance optimization. Each category is handled differently to maximize efficiency:

* **New Writes (Inserts)**: These are writes intended to create new key-value pairs. Conceptually, this is also a read-write operation,
but one where the read version is `nil`, signifying the key does not exist. Instead of performing a costly pre-check, the system
optimistically assumes the key is new and relies on the database's unique key constraints (e.g., a primary key) to enforce uniqueness.
If the key already exists, the database will reject the insert, causing the transaction to fail. This is an efficient trade-off, 
as collisions on new keys are typically infrequent.

* **Blind Writes (Write-Only)**: These occur when a transaction writes to a key without first reading it. The endorser is "blind" to
the key's current version. The version calculation is deferred to the commit phase (Task 3), where the current version is fetched
from the database. If the key does not exist, it is moved to the list of new writes. Otherwise, the existing version is
incremented by 1.

* **Non-Blind Writes (Read-Modify-Write)**: These are standard updates where a key is read before being written. Since the version
is known from the read phase, the new version is calculated by simply incrementing the read version by one.

**d. Enqueueing for Validation:** Once the `preparedTransactions` struct is fully populated, it is enqueued into the `preparedTxs` channel for the Validator task.

### Task 2. Validating Transaction Read-Sets

This task performs the core MVCC logic. It consumes `preparedTransactions` from its input channel, checks the read-sets against the database, and 
filters out any transactions that are invalidated by the check.

**a. Dequeueing Prepared Transactions:** The [validator](https://github.com/hyperledger/fabric-x-committer/blob/main/service/vc/validator.go) reads a `preparedTransactions` object from the `preparedTxs` queue.

**b. Validating Reads:** For performance, instead of fetching each key's version individually, the validator invokes a stored procedure for each namespace. 
This procedure takes the entire read-set for that namespace as input and performs the version validation within the database itself, returning only the 
reads that are invalid due to version discrepancies. Any read identified as invalid by the stored procedure is marked accordingly.

**c. Identifying Invalid Transactions:** Using the `readToTxIDs` map, the validator identifies all transaction IDs associated with any invalid reads. 
The status of these transactions is marked as aborted (e.g., `MVCC_CONFLICT`).

**d. Constructing Validated Writes:** The validator creates a `validatedTransactions` object. This struct contains only the writes from transactions that 
were *not* invalidated. The writes from invalid transactions are discarded.

```go
// validatedTransactions contains the writes of valid transactions and the
// status of invalid ones.
type validatedTransactions struct {
    validTxNonBlindWrites transactionToWrites
    validTxBlindWrites    transactionToWrites
    newWrites             transactionToWrites
    invalidTxStatus       map[TxID]protoblocktx.Status
    txIDToHeight          transactionIDToHeight
}
```

**e. Enqueueing for Commit:** The `validatedTransactions` object is enqueued into the `validatedTxs` channel for the Committer task.

### Task 3. Committing Valid Transactions to the Database


This final task consumes `validatedTransactions`, performs final write preparations, commits the valid data to the database, and
reports the final status of all transactions back to the Coordinator.

**a. Dequeueing Validated Transactions:** The [committer](https://github.com/hyperledger/fabric-x-committer/blob/main/service/vc/committer.go) reads a `validatedTransactions` object from the `validatedTxs` queue.

**b. Resolving Blind Writes:** Before committing, the committer must resolve the "blind writes". It fetches the current versions of keys in 
the `validTxBlindWrites` set from the database. This allows it to categorize each blind write as either an update to an existing key or 
an insert of a new key. For blind writes that are updates, the fetched version from the store is incremented by one to become the new 
version for the write.

**c. Assembling the Commit Batch:** The committer aggregates all valid writes (non-blind, resolved blind, and new) and groups them by namespace.
This allows for efficient, batched commits to the underlying database tables. The final transaction statuses for both valid and invalid 
transactions are compiled into a `TransactionsStatus` message.

```go
// statesToBeCommitted groups all writes by namespace for the final database commit.
type statesToBeCommitted struct {
    updateWrites namespaceToWrites
    newWrites    namespaceToWrites
    batchStatus  *protoblocktx.TransactionsStatus
    txIDToHeight transactionIDToHeight
}
```

**d. Committing to Database:** The committer executes the database transactions to apply the writes. `updateWrites` are applied via the 
`update_ns_${NAMESPACE_ID}` procedure, while `newWrites` are processed by the `insert_ns_${NAMESPACE_ID}` procedure. Concurrently, 
the transaction statuses from the `batchStatus` are recorded in the `tx_status` table using the `insert_tx_status` stored procedure. 
Both `insert_ns_${NAMESPACE_ID}` (for `newWrites`) and `insert_tx_status` can return violating states due to primary key constraint violations.
If this happens, the writes and/or statuses for the corresponding transactions are removed from the batch, their statuses are updated 
(e.g., to reflect a duplicate), and the commit is retried with the modified, smaller batch. This retry loop continues until the commit succeeds.

**e. Reporting Status:** After the commit is successful, the `batchStatus` is sent to the `txsStatus` channel, which relays the information 
back to the Coordinator, completing the workflow for the transaction batch.

## 6. gRPC Service API

The VC service exposes a [gRPC API](https://github.com/hyperledger/fabric-x-committer/blob/main/service/vc/validator_committer_service.go) (`ValidationAndCommitServiceClient`) for the Coordinator and other system components. The methods are:

```go
StartValidateAndCommitStream(ctx context.Context, opts ...grpc.CallOption) (ValidationAndCommitService_StartValidateAndCommitStreamClient, error)
```

 * This API is the primary bidirectional streaming RPC for transaction processing. The Coordinator streams transaction batches to the VC service,
 which processes them through its internal pipeline and streams back their final statuses.

```go
SetLastCommittedBlockNumber(ctx context.Context, in *protoblocktx.BlockInfo, opts ...grpc.CallOption) (*Empty, error)
```

 * This API allows the Sidecar via Coordinator to inform the VC service
about the latest block number that has been successfully committed. This is used during recovery.

```go
GetLastCommittedBlockNumber(ctx context.Context, in *Empty, opts ...grpc.CallOption) (*protoblocktx.LastCommittedBlock, error)
```

 * This API retrieves the last committed block number set by the Sidecar.

```go
GetTransactionsStatus(ctx context.Context, in *protoblocktx.QueryStatus, opts ...grpc.CallOption) (*protoblocktx.TransactionsStatus, error)
```

 * This API provides a way to query the final status of one or more transactions by their IDs, by looking them up in the `tx_status` table.

```go
GetNamespacePolicies(ctx context.Context, in *Empty, opts ...grpc.CallOption) (*protoblocktx.NamespacePolicies, error)
```

 * This API fetches the endorsement policies associated with various namespaces.

```go
GetConfigTransaction(ctx context.Context, in *Empty, opts ...grpc.CallOption) (*protoblocktx.ConfigTransaction, error)
```

 * This API retrieves the latest system configuration transaction.

```go
SetupSystemTablesAndNamespaces(ctx context.Context, in *Empty, opts ...grpc.CallOption) (*Empty, error)
```

 * This API is used to initialize the required system tables (like `tx_status`) and stored procedures in the database when the system is first set up.

## 7. Failure and Recovery

The VC service is designed to be stateless regarding the pipeline itself, relying on the Coordinator and the database for state management and recovery.

### A. State Persistence

The ultimate state of the system is the data committed to the database. The Coordinator is responsible for tracking which transaction batches have been 
sent to the VC service and which have been acknowledged with a final status.

### B. Restart and Recovery Procedure

If the VC service fails and restarts, its internal channels (`preparedTxs`, `validatedTxs`) will be empty. Recovery is managed by the Coordinator:

1.  **Reconnect:** Upon restart, the VC service re-establishes its gRPC server and awaits a connection from the Coordinator.
2.  **Coordinator-led Recovery:** The Coordinator maintains a list of transactions sent to each VC service. Upon detecting a broken stream with a failed VC service, it will resubmit
any transaction batches for which it has not received a final `TransactionsStatus` acknowledgement to another available VC service.
3.  **Idempotent Resubmission of Batches:** This resubmission process is designed to be idempotent to handle duplicate transactions that may arise from recovery scenarios:
    * **Source of Duplicates:** Duplicates can occur if a VC service fails and the Coordinator resubmits its pending transactions to another VC. They can also arise from transient
    network issues causing the same transaction to appear in multiple batches, which may later be merged by the VC service.
    * **Handling Duplicates:** The system handles these duplicates at different stages. First, during the preparation phase (Task 1), any obvious duplicates within a single
    processed batch are detected and ignored. More critically, duplicates arising from resubmission are handled at commit time (Task 3). When the committer attempts to 
    store the final status in the `tx_status` table, it checks if a record for that specific txID, block number, and transaction index already exists. If it does, the service
    identifies it as a resubmission, reuses the already committed status, and does not re-process the transaction. This ensures that the commit operation is idempotent 
    and prevents incorrect validation or duplicate writes. If a transaction is a resubmission, its correct, original status is retrieved from the `tx_status` table.
4.  **Resume Operation:** Once the Coordinator has re-sent any necessary batches and they have been processed idempotently, the system seamlessly resumes normal operation.
