<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Fabric-X Committer Architecture

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture Diagram](#2-architecture-diagram)
3. [Service Components](#3-service-components)
4. [State Management](#4-state-management)
5. [Failure Recovery](#5-failure-recovery)

## 1. Overview

The Fabric-X Committer is built around six core services, each designed with specific responsibilities in the transaction processing pipeline. The services are categorized into stateful and stateless components, with careful consideration given to scalability and fault tolerance.

These six services are: **Sidecar**, **Coordinator**, **Verifier**, **Validator-Committer (VC)**, **Query Service**, and **Database Cluster**.

The architecture achieves high throughput through a sophisticated pipelined design that enables parallel processing of conflict-free transactions while maintaining deterministic outcomes and strong consistency guarantees. Each service plays a specific role: the Sidecar acts as the entry point for blocks from the Ordering Service, the Coordinator orchestrates the validation flow using dependency graph analysis, Verifier services perform parallel signature verification, Validator-Committer (VC) services execute final MVCC validation and commit operations against the database, and the Query Service provides read-only access to the committed world state for clients and endorsers.

## 2. Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────┐
│                        Ordering Service                         │
│                       (External Component)                      │
└────────────────────────────────┬────────────────────────────────┘
                                 │
                                 │ Fetch Blocks
                                 ▼
                        ┌────────────────┐
                        │    Sidecar     │ (1 instance)
                        │   (Stateful)   │
                        │  - Block Store │
                        │  - Query API   │
                        └────────┬───────┘
                                 │
                                 │ Relay Blocks
                                 ▼
                        ┌────────────────┐
                        │  Coordinator   │ (1 instance)
                        │  (Stateless)   │
                        │  - Dep Graph   │
                        │  - Dispatcher  │
                        └───┬────────┬───┘
                            │        │
               ┌────────────┘        └────────────┐
               │                                  │
               │ Verify Signatures                │ Validate & Commit
               ▼                                  ▼
        ┌─────────────┐                    ┌─────────────┐
        │  Verifier   │ (2-3 instances)    │     VC      │ (3-6 instances)
        │ (Stateless) │                    │ (Stateless) │
        │  - Policy   │                    │  - Prepare  │
        │    Cache    │                    │  - Validate │
        └─────────────┘                    │  - Commit   │
                                           └──────┬──────┘
                                                  │
                 Clients/Endorsers                │ Read/Write
                         │                        │
                         │ State Queries          │
                         ▼                        │
                ┌──────────────────┐              │
                │  Query Service   │ (2+ inst.)   │
                │   (Stateless)    │              │
                └────────┬─────────┘              │
                         │                        │
                         │ Read Only              │
                         ▼                        ▼
                ┌─────────────────────────────────────┐
                │       Database Cluster              │ (6-9 nodes)
                │          (Stateful)                 │
                │  - World State   - Tx Status        │
                │  - Policies      - Configuration    │
                └─────────────────────────────────────┘
```

### Component Interactions

1. **Sidecar** fetches blocks from the Ordering Service and relays them to the Coordinator.
2. **Coordinator** dispatches transactions to Verifiers (signature verification) and VCs (MVCC validation and commit) in a pipelined fashion — verification and commit of independent transaction sets proceed concurrently.
3. **VC services** read/write the database cluster with client-side load balancing, then return statuses to the Coordinator.
4. **Coordinator** relays statuses back to the Sidecar, which delivers committed blocks and notifications to clients.

The **Query Service** operates independently of the commit pipeline. Clients and endorsers query world state directly through the Query Service, which reads from the database.

## 3. Service Components

### 3.1 Sidecar Middleware

The Sidecar acts as the critical bridge between the Hyperledger Fabric-X Ordering Service and the Committer's internal processing pipeline. It is responsible for fetching blocks sequentially from the Ordering Service, performing initial transaction validation to filter out malformed transactions, and maintaining a durable local block store on the file system.

Beyond block ingestion, the Sidecar serves as the delivery endpoint for clients, providing committed blocks to registered applications and offering a notification service for transaction status updates. It also exposes query APIs that allow clients to fetch historical blocks and transactions directly from the block store.

**Key Characteristics:**

- **State**: Stateful - maintains a local block store on the file system
- **Cardinality**: Single instance per deployment (critical single point)
- **Scalability**: Vertical scaling only

**Reference**: See [sidecar.md](sidecar.md) for detailed documentation.

### 3.2 Coordinator Service

The Coordinator serves as the central orchestrator of the entire validation and commit pipeline. Its primary task is the use of a transaction dependency graph to identify conflict-free transactions that can be processed in parallel, dramatically improving throughput while maintaining deterministic outcomes.

The Coordinator receives blocks from the Sidecar, constructs and maintains the dependency graph, and intelligently dispatches transactions to downstream services. It manages connections to all Verifier and VC service instances, performing load balancing and automatic failover when services become unavailable. As transactions complete validation and commit, the Coordinator aggregates their final statuses and relays them back to the Sidecar for client delivery.

**Key Characteristics:**

- **State**: Stateless - all persistent state stored in the database via VC service
- **Cardinality**: Single instance per deployment (critical single point)
- **Scalability**: Vertical scaling only

**Reference**: See [coordinator.md](coordinator.md) for detailed documentation.

### 3.3 Verifier Service (Signature Verification)

The Verifier service specializes in validating transaction signatures against namespace endorsement policies. It maintains an in-memory cache of policies for fast lookup, with the database serving as the authoritative source. The service performs parallel signature verification across multiple worker threads, making it highly efficient for high-throughput scenarios. Note that transaction structure validation is performed by the Sidecar.

**Key Characteristics:**

- **State**: Stateless - policies cached in memory, source of truth in database
- **Cardinality**: Multiple instances (typically 2-3 for redundancy and load distribution)
- **Scalability**: Horizontal and vertical scaling

**Reference**: See [verification-service.md](verification-service.md) for detailed documentation.

### 3.4 Validator-Committer (VC) Service

The VC service executes the final stages of transaction processing through a sophisticated three-stage pipeline: Prepare, Validate, and Commit. During the Prepare stage, transactions are structured for efficient validation. The Validate stage performs Multi-Version Concurrency Control (MVCC) checks against the database to detect read-write conflicts. Finally, the Commit stage writes valid transactions to the database and persists their final status.

This service is stateless, with all state maintained in the database. The three-stage pipeline operates entirely on transient in-memory data structures. VC services are designed to be horizontally scalable, with typical deployments running 3-6 instances. These instances are often co-located with database nodes to minimize network latency for database operations.

Each VC instance connects to all database nodes in the cluster and performs client-side load balancing, ensuring efficient utilization of database resources.

**Key Characteristics:**

- **State**: Stateless - all state persisted in the database
- **Cardinality**: Multiple instances (typically 3-6, often co-located with database nodes)
- **Scalability**: Horizontal and vertical scaling

**Reference**: See [validator-committer.md](validator-committer.md) for detailed documentation.

### 3.5 Query Service

The Query Service provides read-only access to the committed world state stored in the database. It serves key-value queries for each namespace, enabling clients and endorsers to read the current state without going through the commit pipeline. This is distinct from the Sidecar's query APIs, which serve block and transaction data from the local block store.

The Query Service connects directly to the database cluster to read namespace tables. It does not need to be co-located with database nodes since it performs read-only operations and should not impact the performance of the commit path.

**Key Characteristics:**

- **State**: Stateless - reads world state from the database
- **Cardinality**: Multiple instances (minimum 2, scale based on endorser count and query throughput)
- **Scalability**: Horizontal and vertical scaling

**Reference**: See [query-service.md](query-service.md) for detailed documentation.

### 3.6 Database Cluster

The database cluster serves as the system's persistent storage layer, maintaining the world state for all namespaces and the final status of every transaction. The system supports two database options, each optimized for different deployment scenarios.

**YugabyteDB** is recommended for production deployments on commodity hardware. It is a distributed SQL database built on top of PostgreSQL, designed specifically for cloud-native deployments. YugabyteDB can achieve throughput exceeding 100,000 transactions per second on standard server hardware through its distributed architecture and automatic sharding capabilities.

**PostgreSQL** is also supported for production use, but requires high-performance flash storage arrays to achieve comparable throughput. Without flash storage, PostgreSQL cannot match the 100k+ TPS performance of YugabyteDB due to I/O limitations. However, for organizations with existing PostgreSQL infrastructure and flash storage investments, it remains a viable option.

**Key Characteristics:**

- **State**: Stateful - stores all committed world state and transaction statuses
- **Cardinality**: Multiple nodes (typically 6-9 for high availability)
- **Scalability**: Horizontal and vertical scaling
- **Consistency**: Raft consensus (YugabyteDB) or configured replication mode (PostgreSQL)

## 4. State Management

State management in Fabric-X Committer is carefully designed to minimize persistent state while ensuring correctness and enabling efficient recovery. The architecture distinguishes between persistent state that must survive service restarts and transient state that can be safely discarded.

**Persistent State** is maintained in only two locations. The Sidecar maintains a block store on the local file system, which serves as a durable record of all blocks received from the Ordering Service. While this block store can be reconstructed from the Ordering Service if lost, maintaining it locally enables fast query responses and reduces load on the Ordering Service. The Database cluster stores all committed world state (key-value pairs for each namespace), transaction statuses, namespace endorsement policies, and system configuration. This makes the database the authoritative source of truth for the entire system.

**Transient State** exists only in memory and is reconstructed on service restart. The Coordinator maintains an in-memory dependency graph for the current block being processed and tracks pending transactions across Verifier and VC services. This state is rebuilt from scratch as new blocks arrive after restart. Verifier services cache namespace policies in memory for fast lookup—policies are reloaded from the database on restart. VC services maintain in-memory pipeline buffers for the Prepare, Validate, and Commit stages, which hold transactions temporarily as they flow through the pipeline. These buffers are emptied and rebuilt during normal operation.

## 5. Failure Recovery

The system handles failures in any service and ensures correctness.

**Verifier Failure.** The Coordinator tracks transactions per Verifier instance. If a Verifier fails, the Coordinator detects this and resubmits its pending transactions to an available replica. To prevent duplicate processing from transient issues, the Coordinator ignores late responses for transactions that have already been reassigned. See [Verifier Error Handling and Recovery](verification-service.md#7-error-handling-and-recovery) for more details.

**Validator-Committer Failure.** Similarly, the Coordinator resubmits pending transactions from a failed VC service to another replica. To prevent incorrect validation or duplicate writes from this resubmission, the commit operation is idempotent. When a VC service attempts to store a transaction's status, it first checks if a record matching the txID, block number, and transaction index already exists. If a match is found, the service reuses the existing committed status instead of reprocessing the transaction. See [VC Failure and Recovery](validator-committer.md#7-failure-and-recovery) for more details.

**Coordinator Failure.** The Sidecar periodically stores the last fully committed block number in the database. After a restart, the Coordinator reads this number to determine the next expected block from the Sidecar, which then begins fetching blocks from that point. Because transactions can be committed across blocks, it is possible some transactions in a fetched block are already committed. The idempotent design of the VC services handles this: they detect existing transaction statuses (via txID, block number, and index) and return the stored status without recommitting. See [Coordinator Failure and Recovery](coordinator.md#7-failure-and-recovery) for more details.

**Sidecar Failure.** When the Sidecar restarts, it gets the next expected block number from the Coordinator and compares it to the last block in its local store. If there is a gap, the Sidecar fetches the missing blocks from the Ordering Service and their transaction statuses from the state database to update its store. It then resumes fetching new blocks from the Ordering Service. See [Sidecar Failure and Recovery](sidecar.md#7-failure-and-recovery) for more details.

**Query Service Failure.** The Query Service is stateless and holds no persistent state. On restart, it reconnects to the database and resumes serving queries immediately. Clients experience a brief interruption and can retry against another Query Service instance. See [Query Service Error Handling and Recovery](query-service.md#error-handling-and-recovery) for more details.

**Database Node Failure.** Database node failures are handled by shard replication; if a shard fails, its replicas remain available. A catastrophic failure or corruption of the entire state database requires rebuilding the state by reprocessing all blocks from the Ordering Service. 
**Multiple Service Failures.** When multiple services fail simultaneously, recovery follows the same per-service procedures described above. The idempotent design of the VC services and the checkpoint-based recovery of the Coordinator and Sidecar ensure that even concurrent failures do not lead to data corruption or duplicate commits. Each service recovers independently upon restart, and the system converges to a consistent state as services come back online.
