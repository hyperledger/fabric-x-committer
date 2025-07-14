<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Coordinator Service

1.  [Overview](#1-overview)
2.  [Core Responsibilities](#2-core-responsibilities)
3.  [Configuration](#3-configuration)
4.  [Workflow Details](#4-workflow-details)
    * [Step 1. Block Ingestion](#step-1-block-ingestion)
    * [Step 2. Dependency Analysis & Parallel Validation](#step-2-dependency-analysis--parallel-validation)
    * [Step 3. Signature Verification](#step-3-signature-verification)
    * [Step 4. Final Validation and Commit](#step-4-final-validation-and-commit)
    * [Step 5. Status Aggregation and Feedback Loop](#step-5-status-aggregation-and-feedback-loop)
    * [Step 6. Post-Commit Processing](#step-6-post-commit-processing)
5.  [Dependency Graph Management](#5-dependency-graph-management)
    * [A. Dependency Types](#a-dependency-types)
    * [B. Identifying Dependency-Free Transactions](#b-identifying-dependency-free-transactions)
6.  [gRPC Service API](#6-grpc-service-api)
7.  [Failure and Recovery](#7-failure-and-recovery)

## 1. Overview

The Coordinator service acts as the central orchestrator of the transaction validation and commit pipeline. It sits between the
Sidecar and a collection of specialized verification, validation and commit services. Its primary role is to manage the complex 
flow of transactions, from initial receipt to final status reporting, by leveraging a transaction dependency graph to maximize 
parallel processing while ensuring deterministic outcomes.

## 2. Core Responsibilities

The [Coordinator](https://github.com/hyperledger/fabric-x-committer/tree/main/service/coordinator) service performs five main tasks:

1.  **Receive Blocks:** Ingests blocks of transactions from the Sidecar service.
2.  **Manage Dependencies:** Utilizes a `Dependency Graph Manager` to construct and maintain a directed acyclic graph (DAG) of transaction dependencies. 
This is the core mechanism for identifying which transactions can be safely processed in parallel.
3.  **Dispatch for Signature Verification:** Forwards dependency-free transactions to a `Signature Verifier Manager`, which distributes the workload
among available `Signature Verifier` services for structural validation and signature checks.
4.  **Dispatch for Final Commit:** Routes transactions that have passed signature verification to a `Validator-Committer Manager`, which in turn distributes
them to available `Validator-Committer` services for final MVCC checks and commitment to the database.
5.  **Aggregate and Relay Statuses:** Receives final transaction statuses from the `Validator-Committer Manager`, notifies the `Dependency Graph Manager` 
to update the graph, and relays the status for each transaction back to the Sidecar.

## 3. Configuration

The Coordinator service requires the following configuration settings, provided in a standard YAML [configuration file](https://github.com/hyperledger/fabric-x-committer/blob/main/cmd/config/samples/coordinator.yaml):

 - *Signature Verifier Endpoints:* Address(es) of the signature verifier service node(s).
 - *Validator-Committer Endpoints:* Address(es) of the validator-committer service node(s).
 - *Listen Address:* The local address and port where the Coordinator will host its gRPC service.

## 4. Workflow Details

The Coordinator manages a multi-stage, pipelined workflow to process transactions efficiently. This pipeline relies on a series of Go channels
for communication between its internal components.

**Inter-component Communication Channels**

```go
// sender: coordinator sends received transactions to this channel.
// receiver: dependency graph manager receives transactions batch from this channel and construct dependency
//           graph.
coordinatorToDepGraphTxs chan *dependencygraph.TransactionBatch

// sender: dependency graph manager sends dependency free transactions nodes to this channel.
// receiver: signature verifier manager receives dependency free transactions nodes from this channel.
depGraphToSigVerifierFreeTxs chan dependencygraph.TxNodeBatch

// sender: signature verifier manager sends valid transactions to this channel.
// receiver: validator-committer manager receives valid transactions from this channel.
sigVerifierToVCServiceValidatedTxs chan dependencygraph.TxNodeBatch

// sender: validator committer manager sends validated transactions nodes to this channel. For each validator
//         committer server, there is a goroutine that sends validated transactions nodes to this channel.
// receiver: dependency graph manager receives validated transactions nodes from this channel and update
//           the dependency graph.
vcServiceToDepGraphValidatedTxs chan dependencygraph.TxNodeBatch

// sender: validator committer manager sends transaction status to this channel. For each validator committer
//         server, there is a goroutine that sends transaction status to this channel.
// receiver: coordinator receives transaction status from this channel and forwards them to the sidecar.
vcServiceToCoordinatorTxStatus chan *protoblocktx.TransactionsStatus
```

### Step 1. Block Ingestion

The process begins when the Coordinator receives a block from the Sidecar. The Sidecar has already performed an initial pass to mark any 
transactions with duplicate identifiers within its active, in-flight transaction list. The Coordinator then places these transactions 
onto the `coordinatorToDepGraphTxs` channel.

### Step 2. Dependency Analysis & Parallel Validation

The [`Dependency Graph Manager`](https://github.com/hyperledger/fabric-x-committer/tree/main/service/coordinator/dependencygraph)
consumes transactions from the `coordinatorToDepGraphTxs` channel and builds a dependency graph. This process
identifies all transactions that are "dependency-free" and can be immediately scheduled for validation. These dependency-free transaction
nodes are sent to the `depGraphToSigVerifierFreeTxs` channel.

### Step 3. Signature Verification

The [`Signature Verifier Manager`](https://github.com/hyperledger/fabric-x-committer/blob/main/service/coordinator/signature_verifier_manager.go) receives dependency-free nodes from the `depGraphToSigVerifierFreeTxs` channel and acts as a load balancer,
distributing the transactions across a pool of available `Signature Verifiers`. The manager also keeps track of all requests sent to each 
verifier service. If a service fails, the manager forwards any pending requests to another available service to ensure no transactions are lost.
These services perform preliminary checks, including signature
verification and structural validation. If a transaction fails these checks, the `Signature Verifier` returns an invalid status. The `Signature Verifier Manager`
then sets this invalid status directly on the transaction node. It is important to note that all transactions, both valid and those marked as invalid, 
are passed along to the next stage via the `sigVerifierToVCServiceValidatedTxs` channel. This ensures that even invalid transactions reach the 
`Validator-Committer` service, which will see the pre-set invalid status, skip its own validation, and simply record the final invalid status in the database.

### Step 4. Final Validation and Commit

The [`Validator-Committer Manager`](https://github.com/hyperledger/fabric-x-committer/blob/main/service/coordinator/validator_committer_manager.go) receives verified transactions from the `sigVerifierToVCServiceValidatedTxs` channel and routes them to
available `Validator-Committer` services. The manager also keeps track of all requests sent to each verifier service. If a service fails, the manager
forwards any pending requests to another available service to ensure no transactions are lost.  The `Validator-Committer` services execute their own
three-phase pipelined process (Prepare, Validate, Commit) to perform final MVCC checks against the database and commit the results.

### Step 5. Status Aggregation and Feedback Loop

As transactions are committed or aborted by the `Validator-Committer` services, their final statuses are sent back to the `Validator-Committer Manager`.
This manager then forwards the results to two separate channels:
1.  The final node status (committed or aborted) is sent to the `vcServiceToDepGraphValidatedTxs` channel. The `Dependency Graph Manager` consumes
this to update the graph, which may resolve dependencies for other waiting transactions.
2.  The raw transaction status is sent to the `vcServiceToCoordinatorTxStatus` channel. The Coordinator consumes this and forwards the statuses
to the Sidecar for final aggregation and delivery to clients.

### Step 6. Post-Commit Processing
If a committed transaction creates or updates a namespace, a post-commit process is triggered to update the system's policies. The `Validator-Committer Manager`,
in conjunction with a [`Policy Manager`](https://github.com/hyperledger/fabric-x-committer/blob/main/service/coordinator/policy_manager.go), ensures that all `Signature Verifier` services are updated with the new endorsement policy for that namespace. 
This update is not performed immediately via a separate call. Instead, for efficiency, the policy update is "piggybacked" onto the next validation
request sent to each verifier. The verifier then updates its internal policy list before processing the accompanying transactions.

## 5. Dependency Graph Management

The core component enabling parallel processing is the transaction dependency graph, managed by the `Dependency Graph Manager` on behalf of the Coordinator.
This directed acyclic graph (DAG) ensures deterministic outcomes despite concurrent execution.

### A. Dependency Types

An edge from a later transaction ($T_j$) to an earlier transaction ($T_i$) indicates that $T_i$ must be finalized before $T_j$ can be validated. The graph tracks three types of dependencies:

1.  **Read-Write Dependency ($T_{i}\xleftarrow{rw\mbox{(}k\mbox{)}}T_{j}$):** $T_i$ writes to key `k`, and a later transaction $T_j$ reads the *previous* version of `k`.
If $T_i$ is valid, $T_j$ must be invalid because it read a stale value.
2.  **Write-Read Dependency ($T_{i}\xleftarrow{wr\mbox{(}k\mbox{)}}T_{j}$):** $T_i$ reads key `k`, and a later transaction $T_j$ writes to `k`. This dependency is used
to enforce commit order. $T_j$ cannot be committed before $T_i$ is finalized, otherwise $T_i$'s read would become stale, violating the original block order.
3.  **Write-Write Dependency ($T_{i}\xleftarrow{ww\mbox{(}k\mbox{)}}T_{j}$):** Both $T_i$ and $T_j$ write to the same key `k`. This dependency ensures $T_j$ is not 
committed before $T_i$, preventing $T_j$'s write from being overwritten and lost.

### B. Identifying Dependency-Free Transactions

Transactions that have no outgoing edges in the graph (an out-degree of zero) have no outstanding dependencies. These are the transactions that the Coordinator
identifies and sends out for parallel validation and commit.

## 6. gRPC Service API

The Coordinator exposes a gRPC API (`CoordinatorClient`) that is primarily used by the Sidecar for sending blocks and managing state.

```go
BlockProcessing(ctx context.Context, opts ...grpc.CallOption) (Coordinator_BlockProcessingClient, error)
```
  * This API starts bidirectional streaming RPC. The Sidecar streams blocks to the Coordinator, which in turn streams back the final status of each transaction within those blocks.

```go
SetLastCommittedBlockNumber(ctx context.Context, in *protoblocktx.BlockInfo, opts ...grpc.CallOption) (*Empty, error)
``` 
  * This API allows the Sidecar to inform the Coordinator about the latest block number that has been successfully and sequentially committed.
  The Coordinator then passes this to the Validator-Committer service to store in the state database. This is used for state synchronization during recovery.

```go
GetLastCommittedBlockNumber(ctx context.Context, in *Empty, opts ...grpc.CallOption) (*protoblocktx.LastCommittedBlock, error)
```
  * This API retrieves the last committed block number by querying the Validator-Committer service, which reads the data from the state database.

```go
GetNextExpectedBlockNumber(ctx context.Context, in *Empty, opts ...grpc.CallOption) (*protoblocktx.BlockInfo, error)
```
  * This API is used by the Sidecar on startup/recovery to ask the Coordinator which block it should fetch next from the ordering service.

```go
GetTransactionsStatus(ctx context.Context, in *protoblocktx.QueryStatus, opts ...grpc.CallOption) (*protoblocktx.TransactionsStatus, error)
```
  *  This API queries the final status of specific transactions by their IDs. The Coordinator forwards this query to a Validator-Committer service, which reads the status from the database.

```go
GetConfigTransaction(ctx context.Context, in *Empty, opts ...grpc.CallOption) (*protoblocktx.ConfigTransaction, error)
```
  * This API retrieves the latest system configuration transaction by querying a Validator-Committer service, which reads it from the database.

```go
NumberOfWaitingTransactionsForStatus(ctx context.Context, in *Empty, opts ...grpc.CallOption) (*WaitingTransactions, error)
```
  * This API returns the number of transactions that are currently in the pipeline awaiting a final status.

## 7. Failure and Recovery

The system is designed to be resilient to Coordinator failures.

* **State Persistence:** The Sidecar periodically stores the block number of the last sequentially completed block in the state database.
* **Restart Procedure:** When the Coordinator restarts after a failure, it reads this last committed block number from the database.
* **Resuming Operation:** It instructs the Sidecar to begin fetching blocks starting from the next expected block number.
* **Idempotent Commits:** It is possible that transactions within these newly fetched blocks have already been committed by `Validator-Committer`
services before the failure. This is handled gracefully because the `Validator-Committer` services are designed to be idempotent. 
When a transaction is resubmitted, the service checks if a status for that txID, block number, and transaction index already exists. 
If so, it returns the existing status without re-committing the transaction, ensuring data consistency.
