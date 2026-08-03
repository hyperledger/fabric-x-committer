<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Deployment Guide for Fabric-X Committer

## Table of Contents

1. [Hardware Requirements](#1-hardware-requirements)
2. [Reference Deployment](#2-reference-deployment)
3. [Scaling Guidance](#3-scaling-guidance)
4. [Startup and Shutdown Order](#4-startup-and-shutdown-order)

## 1. Hardware Requirements

Resource requirements depend on the target throughput, transaction complexity, and workload characteristics. The specifications below are sized for achieving a minimum of 50,000 transactions per second with a simple workload of read-write transactions, each involving 2 key-value pairs of 32 bytes key and 32 bytes value. More complex workloads (larger key-value sizes, higher read/write set counts, complex endorsement policies) will require additional resources.

Recommended specifications:

- **Sidecar Node** (1 node): 32 CPU cores, 8 GB RAM, high-performance NVMe SSD (for block store)
- **Coordinator Node** (1 node): 32 CPU cores, 8 GB RAM
- **Verifier Nodes** (3 nodes): 32 CPU cores, 8 GB RAM
- **VC + Database Nodes** (6-9 nodes): 32 CPU cores, 32 GB RAM, high-performance NVMe SSD (for database storage)
- **Query Service Nodes** (2+ nodes): 32 CPU cores, 8 GB RAM
- **Network**: 10 Gbps links between all nodes

The Sidecar requires high-performance NVMe storage because block store operations are I/O-intensive — blocks must be written durably and read back with minimal latency to keep the pipeline fed. Database nodes similarly require NVMe SSDs to sustain the write throughput demanded by concurrent MVCC validation and commit operations, where multiple VC instances drive parallel writes across distributed tablets. For the stateless services (Coordinator, Verifier, VC), 8 GB RAM is sufficient since they hold only in-flight request state. Database nodes need 32 GB RAM to maintain effective in-memory caching of frequently accessed world state data and tablet metadata, reducing disk I/O for read-heavy MVCC validation.

Actual requirements should be validated through benchmarking with your workload on the target hardware and topology.

## 2. Reference Deployment

The following topology is a reference starting point based on micro-benchmarks, not a prescription. Adjust based on benchmarking with your actual workload.

```
┌──────────────────────────────────────────────────────────────┐
│                       Ordering Service                       │
│                     (External Component)                     │
└──────────────────────────┬───────────────────────────────────┘
                           │
                           │ Fetch Blocks
                           ▼
            ┌──────────────────────────────┐
            │           Sidecar            │
            │          (Stateful)          │
            │     - Block Store            │
            │     - Query API              │
            └──────────────┬───────────────┘
                           │
                           │ Relay Blocks
                           ▼
            ┌──────────────────────────────┐
            │         Coordinator          │
            │         (Stateless)          │
            │   - Dependency Graph         │
            │   - Load Balancer            │
            └──────────────┬───────────────┘
                           │
              ┌────────────┴────────────┐
              │                         │
              │ Verify Signatures       │ Validate & Commit
              ▼                         ▼
  ┌───────────────────────┐    ┌──────────────────────────────────┐
  │   Verifier-1          │    │  VC-1 (co-located with DB-1)     │
  │   (Stateless)         │    │  (Stateless)                     │
  ├───────────────────────┤    ├──────────────────────────────────┤
  │   Verifier-2          │    │  VC-2 (co-located with DB-2)     │
  │   (Stateless)         │    │  (Stateless)                     │
  ├───────────────────────┤    ├──────────────────────────────────┤
  │   Verifier-3          │    │  VC-3 (co-located with DB-3)     │
  │   (Stateless)         │    │  (Stateless)                     │
  └───────────────────────┘    ├──────────────────────────────────┤
                               │  VC-4 (co-located with DB-4)     │
                               │  (Stateless)                     │
                               ├──────────────────────────────────┤
                               │  VC-5 (co-located with DB-5)     │
                               │  (Stateless)                     │
                               ├──────────────────────────────────┤
                               │  VC-6 (co-located with DB-6)     │
                               │  (Stateless)                     │
                               └────────────┬─────────────────────┘
                                            │
                                            │ Read/Write
                                            │ (All VCs connect to all DB nodes)
                                            ▼
                  ┌─────────────────────────────────────────────┐
                  │         Database Cluster (9 nodes)          │
                  │                 (Stateful)                  │
                  ├─────────────────────────────────────────────┤
                  │  DB-1  │  DB-2  │  DB-3  │  DB-4  │  DB-5   │
                  │  DB-6  │  DB-7  │  DB-8  │  DB-9            │
                  ├─────────────────────────────────────────────┤
                  │  - World State (all namespaces)             │
                  │  - Transaction Status                       │
                  │  - Namespace Policies                       │
                  │  - System Configuration                     │
                  └─────────────────────────────────────────────┘
```

**Why these numbers:**

- **Sidecar (1)**: Single instance by design. The Sidecar processes blocks sequentially from the Ordering Service — there is no benefit to multiple instances. Scale vertically if block ingestion rate is insufficient.
- **Coordinator (1)**: Single instance by design. The Coordinator maintains an in-memory dependency graph that must be consistent, so only one instance can manage it. Scale vertically if dependency graph management becomes a bottleneck.
- **Verifier (3)**: Provides parallel signature verification with redundancy. Three instances tolerate 1-2 failures while continuing to process transactions. Adjust the count based on endorsement policy complexity — more complex policies with more signatures per transaction benefit from additional instances.
- **VC (6, co-located with DB)**: Co-location of VC instances with database nodes minimizes network latency for MVCC operations, reducing round-trip times from milliseconds to microseconds. Each VC instance connects to ALL database nodes via client-side load balancing, so co-location with a subset of nodes is sufficient to gain the latency benefit.
- **Database (9, RF=3)**: A 9-node cluster with replication factor 3 tolerates up to 2 simultaneous node failures without data loss or availability impact. The node count enables distributed query processing across many tablets for high write throughput.
- **Query Service (2+)**: Provides read-only access to committed world state for clients and endorsers. Minimum 2 instances for redundancy. Does not need to be co-located with database nodes since it performs read-only operations and should not impact the commit path performance. Scale based on the number of endorsers and query throughput requirements.

For detailed service descriptions and architecture, see the [Architecture Guide](architecture.md).

## 3. Scaling Guidance

All service endpoints must be pre-configured before deployment. To scale up, start additional pre-configured instances. To scale down, stop the instances no longer needed.

**Retry budget for dynamic scaling.** The Coordinator maintains its connections to the Verifier and Validator-Committer instances (and the Sidecar its connection to the Coordinator) with an exponential-backoff retry loop bounded by the `reconnect.max-elapsed-time` budget (default 15 minutes). If a pre-configured instance stays down longer than that budget — for example, while it is stopped for scaling — the sustained operation gives up. For deployments that stop and start instances at runtime, set `reconnect.max-elapsed-time: 0s` on the relevant client configuration to retry **indefinitely** (never give up), so the Coordinator keeps reconnecting as instances come and go. The other retry parameters (`initial-interval`, `randomization-factor`, `multiplier`, `max-interval`) still apply.

**Reconnection cadence (`reconnect.max-interval`).** The retry delay grows exponentially from `initial-interval` up to `max-interval` (default 10 seconds); once it reaches `max-interval`, every subsequent attempt waits that fixed interval. `max-interval` therefore sets the steady-state cadence at which a client polls a disconnected instance, and bounds how long it may wait before reconnecting to an instance that has come back online — for example, one just started during scale-up. Set `max-interval` with this trade-off in mind: a shorter interval reconnects to new or recovered instances faster at the cost of more frequent retry attempts, while a longer interval reduces retry traffic but slows reconnection.

### 3.1 Scaling Verifier Instances

Verifier services are CPU-bound — their primary work is cryptographic signature verification. Add Verifier instances when CPU utilization on existing instances is consistently saturated. To scale up, start additional pre-configured Verifier instances. To scale down, stop the instances no longer needed.

### 3.2 Scaling VC and Database Instances

VC instances may be co-located with database nodes but do not need to be scaled together. When adding VC capacity, start additional pre-configured VC instances. When adding database capacity, add new database nodes. After adding database nodes, YugabyteDB requires tablet rebalancing to distribute data across the new nodes.

### 3.3 Scaling Database Cluster

When adding database nodes, add them in pairs to maintain balanced data distribution. Configure `table-pre-split-tablets` to match the new tablet server count so that newly created tables are distributed evenly. See [Validator-Committer Configuration](validator-committer.md) for database configuration details.

### 3.4 Scaling Query Service Instances

Query Service instances are stateless and perform read-only queries against the database. Add instances based on the number of endorsers and query throughput requirements. Query Service instances do not need to be co-located with database nodes.

### 3.5 Sidecar and Coordinator

Both the Sidecar and Coordinator are single-instance services. They cannot be scaled horizontally. If either becomes a bottleneck, scale vertically by allocating more CPU cores and memory to the node.

See [Performance Tuning](performance-tuning.md) for configuration parameters and their impact on throughput and latency.

## 4. Startup and Shutdown Order

### Startup Sequence

Start the database cluster first and wait for it to be healthy before starting any services.

1. **Start Database Cluster**
   - Start all database nodes
   - Wait for the cluster to form and achieve quorum
   - Verify cluster health and connectivity
   - For YugabyteDB: ensure all tablet servers are online and the tablet leader election is complete
   - For PostgreSQL: ensure replication is established between primary and replicas

2. **Initialize Database**
   - Run the database initialization command to create system tables and namespaces:
     ```shell
     committer init-db --config <path-to-vc-config> --timeout <duration>
     ```
     **Parameters:**
      - `--config`: Path to the VC service configuration file (required)
      - `--timeout`: Timeout for the initialization operation (default: 5m)

   - This command connects directly to the database and creates:
     - System tables (`tx_status`, `metadata`)
     - System namespaces (`_meta`, `_config`)
     - Required stored procedures
   - This is a one-time administrative operation that must be performed before booting the committer for the first time
   - The command is safe to run multiple times

3. **Provision a YugabyteDB PITR snapshot schedule (required for state snapshots)**

   State snapshots create a native database clone via `CREATE DATABASE ... TEMPLATE`.
   On YugabyteDB, cloning is built on Point-in-Time Recovery and **fails without a
   pre-existing snapshot schedule** on the state database (error: `Could not find
   snapshot schedule for namespace <db>`) — this applies even to as-of-now clones.
   **Without this schedule, snapshots cannot be created.** PostgreSQL deployments do
   not need this step.

   The schedule can only be created with the `yb-admin` CLI (no YSQL statement exists),
   so it is an operator provisioning step performed once against the state database:
   ```shell
   yb-admin --master_addresses <ip1:7100,ip2:7100,ip3:7100> \
     create_snapshot_schedule <interval-minutes> <retention-minutes> ysql.<database>
   ```

   **Keep the interval and retention as short as your operations allow.** The committer
   needs the schedule only to enable cloning, not for a recovery window. Retention is the
   performance lever: a long window pins old SST files (disk growth) and forces DocDB to
   retain more MVCC history (larger, slower scans). A short interval/retention lets
   compaction reclaim history promptly. The schedule's mere existence has no throughput
   cost. Note that while a schedule exists it blocks `DROP DATABASE` of the state database
   and disallows `DROP TABLESPACE` cluster-wide.

4. **Start Services** (any order after database is healthy and initialized)
   - VC Service — connects to database on startup
   - Query Service — connects to database on startup
   - Verifier Service — stateless, loads policies on first request
   - Coordinator Service — connects to VC and Verifier services
   - Sidecar Service — connects to Coordinator and Ordering Service

### Service Dependencies

| Service | Dependencies |
|---------|-------------|
| Database | No dependencies |
| VC Service | Requires Database |
| Query Service | Requires Database |
| Verifier | No dependencies (policies loaded on first request) |
| Coordinator | Requires VC Service and Verifier |
| Sidecar | Requires Coordinator and Ordering Service |

### Graceful Shutdown

Shut down in reverse order to drain work through the pipeline:

1. **Sidecar** — stops ingesting new blocks
2. **Coordinator** — stops dispatching transactions
3. **Verifier and VC Services** — completes in-flight work
4. **Database** — shut down last to ensure all data is persisted
