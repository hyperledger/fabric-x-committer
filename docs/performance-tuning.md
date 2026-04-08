<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Performance Tuning Guide

This guide explains how each configuration parameter affects system performance — throughput, latency, memory, and pipeline flow. All parameters are documented with their sample values in `cmd/config/samples/`.

## Table of Contents

1. [Pipeline Flow Control](#1-pipeline-flow-control)
2. [Identifying the Bottleneck](#2-identifying-the-bottleneck)
3. [Sidecar](#3-sidecar)
4. [Coordinator](#4-coordinator)
5. [Verifier](#5-verifier)
6. [Validator-Committer (VC)](#6-validator-committer-vc)
7. [Query Service](#7-query-service)
8. [Database](#8-database)
9. [Co-location Impact](#9-co-location-impact)
10. [Benchmarking](#10-benchmarking)

## 1. Pipeline Flow Control

The commit pipeline processes transactions through a sequence of stages connected by bounded channels and slot-based limits. These flow controls don't just protect memory — they directly control how much work flows through the pipeline.

```
Orderer → Sidecar → Coordinator → Dependency Graph → Verifier → VC → Database
             ↑            ↑                                      |
             └── status ──└──────────── status ──────────────────┘
```

Each `→` is a bounded channel or gRPC stream. The system uses three types of flow control:

- **Slot-based limits** act as per-transaction semaphores. Slots are acquired before processing and released only when transactions complete downstream. When exhausted, channels fill up and the Sidecar stops pulling blocks.
- **Channel buffers** connect adjacent stages within a process. When full, the producer blocks.
- **gRPC flow control** operates at the transport layer between services. gRPC uses HTTP/2 flow control windows — when a receiver is slow, its window fills up and the sender is blocked from writing more data. This prevents a fast producer (e.g., Sidecar) from overwhelming a slow consumer (e.g., Coordinator) at the network level, independent of the application-level slot and channel limits.

Setting any limit too low starves the pipeline — stages run in lock-step rather than streaming, and throughput drops. Setting limits too high increases memory usage and queuing latency. The goal is finding the balance where the pipeline has enough in-flight work to sustain throughput without excessive queuing.

## 2. Identifying the Bottleneck

Monitor queue length gauges to find the bottleneck. A growing queue means the downstream stage cannot keep up. Tune that stage first. The full list of queue metrics and other observability metrics can be found in the [Metrics Reference](metrics_reference.md). Key queues to watch:

| Queue Metric | Stage | Growing Queue Means |
|-------------|-------|---------------------|
| `sidecar_relay_input_block_queue_size` | Block Ingestion | Coordinator not consuming blocks fast enough |
| `sidecar_relay_waiting_transactions_queue_size` | Relay | Transactions waiting for commit statuses to return |
| `sidecar_relay_output_committed_block_queue_size` | Committed Blocks | Committed blocks backing up; downstream consumers slow |
| `coordinator_sigverifier_input_tx_batch_queue_size` | Signature Verification | Verifiers cannot keep up; add instances or CPU |
| `coordinator_sigverifier_output_validated_tx_batch_queue_size` | Verified → VC | VC services not consuming verified transactions fast enough |
| `coordinator_vcservice_output_validated_tx_batch_queue_size` | VC → Dep Graph | Dependency graph not processing validated results fast enough |
| `coordinator_vcservice_output_tx_status_batch_queue_size` | Status Response | Status responses backing up between VC and Coordinator |
| `vcservice_preparer_input_queue_size` | VC Preparation | Preparer workers saturated |
| `vcservice_validator_input_queue_size` | VC Validation | DB validation queries too slow; check connections or co-location |
| `vcservice_committer_input_queue_size` | VC Commit | DB commit throughput is the bottleneck; most common |
| `vcservice_txstatus_output_queue_size` | VC Status Output | Status responses backing up; Coordinator not consuming fast enough |

## 3. Sidecar

### `waiting-txs-limit`

Maximum number of transactions the Sidecar has sent to the Coordinator and is awaiting status for. The Sidecar acquires one slot per transaction before sending a block. Slots are released only when the Coordinator returns the transaction's final status. When all slots are occupied, the Sidecar blocks on slot acquisition, which causes the internal block channel to fill up, and eventually the Sidecar stops pulling blocks from the ordering service.

This value directly controls how many transactions can be in-flight across the entire pipeline. With a low value (e.g., 100), the Sidecar sends 100 transactions and blocks until all complete — the pipeline runs in lock-step and throughput drops dramatically. With a very high value, more transactions queue across the pipeline, increasing memory and queuing latency. Config blocks trigger a full drain regardless of this value. Default: 20,000,000.

### `ledger.sync-interval`

How often the block store calls `fsync` to durable storage. Every Nth block triggers a full sync; intermediate blocks are written without fsync. Config blocks and file rollovers always sync.

Each fsync is an expensive I/O operation. A low value (e.g., 1) syncs every block, which can bottleneck the block ingestion path. Higher values (100, 500+) significantly improve block append throughput by amortizing I/O cost. The tradeoff is durability — blocks lost on crash are recoverable from the ordering service. Default: 100.

### `channel-buffer-size`

Buffer size for internal Go channels in the Sidecar — block delivery, committed blocks, and status updates. When a channel is full, the producing goroutine blocks until the consumer reads.

A small buffer (e.g., 1-10) tightly couples the Sidecar's internal stages: any slowdown in the relay to the Coordinator immediately stalls block delivery from the orderer. A larger buffer absorbs temporary throughput variations but uses more memory. Default: 100.

### `last-committed-block-set-interval`

How often the Sidecar sends the latest committed block number to the Coordinator. The Coordinator uses this for dependency resolution.

Minimal effect on steady-state throughput. Shorter intervals improve recovery speed after failures. Default: 5s.

### `notification.max-active-tx-ids`

Global limit on active transaction ID subscriptions across all notification streams. When exhausted, new subscriptions are partially rejected.

Too low causes clients to receive rejections under moderate load, forcing retries that increase end-to-end latency. Default: 100,000.

### `notification.max-tx-ids-per-request`

Maximum transaction IDs per single notification request. Requests exceeding this are rejected entirely.

Prevents individual clients from consuming a disproportionate share of the subscription budget in a single call. Default: 1,000.

### `server.max-concurrent-streams`

Maximum concurrent streaming RPCs (Deliver + Notification) per client connection. Each stream holds server resources (goroutines, buffers).

Too low limits client concurrency and can cause connection failures under load. Default: 10.

## 4. Coordinator

### `dependency-graph.num-of-local-dep-constructors`

Number of goroutines that process transaction batches in parallel to construct batch-level dependency graphs. Each worker processes one batch at a time, and output is serialized in FIFO order.

Increasing this parallelizes the CPU work of building dependency graphs. However, since output order is enforced via a condition variable, gains diminish beyond 2-4 workers. Default: 1.

### `dependency-graph.waiting-txs-limit`

Maximum number of transactions in the global dependency graph. The Coordinator acquires one slot per transaction before adding it to the graph. Slots are released when the VC returns validation results. When exhausted, the dependency graph construction blocks, channels fill up, and the Sidecar stops pulling blocks.

The dependency graph is what enables parallel dispatch to Verifier and VC services. A small graph (e.g., 100) means once transactions are dispatched, no new ones enter until results return — creating idle gaps and reducing throughput. A very large graph increases memory for dependency tracking state and queuing latency. Incoming blocks are chunked into batches of `min(waiting-txs-limit, 500)` to prevent a single block from consuming all slots. Default: 100,000.

### `per-channel-buffer-size-per-goroutine`

Base buffer size for internal Go channels connecting the Coordinator's pipeline stages. The actual buffer for each channel is computed as base × number of endpoints (or constructors):

```
Coordinator → DepGraph:  base × num-of-local-dep-constructors
DepGraph → Verifier:     base × number-of-vc-endpoints
Verifier → VC:           base × number-of-verifier-endpoints
VC → DepGraph:           base × number-of-vc-endpoints
VC → Coordinator:        base × number-of-vc-endpoints
```

When a channel is full, the producing stage blocks. A small buffer means any momentary downstream slowdown immediately stalls all upstream stages. A larger buffer absorbs temporary throughput variations but increases memory and queuing latency. With 3 verifiers and 6 VCs, the defaults produce 30-60 item buffers per channel. Default: 10.

## 5. Verifier

### `parallel-executor.parallelism`

Number of goroutines that verify signatures in parallel. Signature verification is CPU-bound. Each stream from the Coordinator creates an independent executor with this many workers.

Set this to match the number of CPU cores available on the Verifier node. Under-setting leaves CPU idle, reducing verification throughput and causing transactions to queue at the Coordinator. Over-setting causes context switching overhead with no throughput gain. The default of 40 assumes a 32+ core machine.

### `parallel-executor.batch-size-cutoff`

Minimum number of verification results to collect before emitting a batch to the Coordinator. Results are buffered until this threshold is reached or `batch-time-cutoff` expires, whichever comes first.

Setting this too low causes many small batches to be emitted, increasing per-batch overhead in channel writes and gRPC communication. Setting this too high delays results — individual transactions wait longer for the batch to fill, increasing end-to-end latency.

| Setting | Throughput | Latency |
|---------|-----------|---------|
| 10-20 | Lower (more frequent, smaller batches) | Lower |
| 50 (default) | Good balance | Moderate |
| 100-200 | Higher (fewer, larger batches) | Higher |

### `parallel-executor.batch-time-cutoff`

Maximum time to wait for a batch to reach `batch-size-cutoff` before emitting a partial batch. This is the latency safety valve — without it, a partially filled batch under low load would wait indefinitely.

Setting this too low defeats batching (batches emit before they can fill). Setting this too high adds latency during periods of low or variable transaction arrival rates.

The default of 10ms ensures results are emitted promptly even under low load. Increase to 50-100ms only if batching efficiency is more important than latency.

### `parallel-executor.channel-buffer-size`

Buffer size for internal channels in the verification pipeline. The actual channel capacity is computed as:

```
capacity = channel-buffer-size × parallelism
```

With the defaults (50 buffer × 40 parallelism = 2,000 capacity). When the input channel is full, the Coordinator's dispatch to the Verifier blocks, which stalls the dependency graph. When the output channel is full, verification workers block, reducing effective parallelism.

Setting this too low causes frequent blocking that reduces the effective parallelism of the verification workers. Setting this too high increases memory usage proportionally. The defaults provide enough buffering for sustained high throughput.

## 6. Validator-Committer (VC)

The VC processes transactions through three pipeline stages: preparation, validation, and commit. Each stage has an independent worker pool. The overall throughput is limited by the slowest stage.

### `resource-limits.max-workers-for-preparer`

Number of goroutines that extract read/write sets from transactions and organize them for validation. Preparation is **CPU-bound** — it parses transaction payloads, builds namespace-to-reads maps, and categorizes writes.

Setting this too low makes the preparer the bottleneck — transactions queue up waiting for preparation while the validator and committer workers sit idle. Setting this higher than needed wastes CPU on goroutine overhead.

The default of 1 is sufficient for most workloads because preparation is fast relative to the database-bound stages. Increase to 2-4 if you observe the preparer queue growing while validator and committer queues are empty.

### `resource-limits.max-workers-for-validator`

Number of goroutines that perform MVCC validation against the database. Each worker calls a stored procedure (`validate_reads_ns_<namespace>`) that checks whether read set versions still match the committed state.

Validation is **database-bound** — each call makes at least one database round-trip per namespace in the transaction's read set. Each active validator worker holds a database connection for the duration of its query.

Setting this too low causes transactions to queue at the validator stage while the database has spare capacity. Setting this too high exhausts the connection pool — workers compete for connections, and the overhead of connection acquisition negates the parallelism benefit.

| Setting | Effect |
|---------|--------|
| 1 (default) | Serialized validation; minimal DB load; may under-utilize DB |
| 2-4 | Parallel validation; higher throughput; needs more DB connections |
| >4 | Diminishing returns unless DB can sustain the concurrent queries |

### `resource-limits.max-workers-for-committer`

Number of goroutines that commit validated transactions to the database using stored procedures (`insert_tx_status`, `insert_ns_<namespace>`, `update_ns_<namespace>`). Each commit involves multiple database round-trips within a transaction: writing transaction status, inserting new keys, and updating existing keys.

The committer is typically the **pipeline bottleneck** because it performs the most database work per transaction. Each active committer worker holds a database connection for the duration of the commit. When transactions complete here, slots are released in the Coordinator's dependency graph and the Sidecar's waiting-txs pool — so committer throughput directly controls how fast the entire pipeline can flow.

Setting this too low starves the pipeline — the Coordinator's dependency graph fills up, backpressure reaches the Sidecar, and block ingestion stalls. Setting this too high overwhelms the database with concurrent transactions, causing contention and increased retry rates.

| Setting | Effect |
|---------|--------|
| 1-2 | Low throughput; database under-utilized; pipeline stalls |
| 10-20 (default: 20) | Good throughput; recommended starting point |
| >20 | Diminishing returns; may overwhelm the database with contention |

### `resource-limits.min-transaction-batch-size`

Minimum number of transactions that must accumulate before the batch is forwarded to the preparation stage. The VC waits for this many transactions or until `timeout-for-min-transaction-batch-size` expires, whichever comes first. Config blocks are always sent immediately regardless of batch size.

Larger batches improve efficiency — stored procedures process keys in bulk, reducing per-transaction overhead. But larger batches also increase latency because early-arriving transactions wait for the batch to fill.

Setting this too high under low transaction rates causes transactions to sit idle until the timeout expires, adding `timeout-for-min-transaction-batch-size` of latency to every batch. Setting this to 1 (default) disables batching entirely — every transaction is forwarded immediately for lowest latency.

| Setting | Throughput | Latency |
|---------|-----------|---------|
| 1 (default) | Lower (no batching benefit) | Lowest |
| 10-50 | Higher (bulk stored procedure operations) | Higher (waits for batch to fill) |

### `resource-limits.timeout-for-min-transaction-batch-size`

Maximum time to wait for a batch to reach `min-transaction-batch-size`. This is the latency safety valve for batching — without it, a partially filled batch under low load would wait indefinitely.

When `min-transaction-batch-size` is 1, this timeout has no effect (batches are sent immediately). When `min-transaction-batch-size` is higher, this timeout determines the worst-case additional latency during low-throughput periods.

The default of 2s pairs with the default batch size of 1 (effectively unused). If you increase the batch size, consider reducing the timeout to 100-500ms to bound the latency impact.

### `database.max-connections`

Maximum number of connections in the database connection pool. The validator and committer stages share this pool (the preparer does not use the database — it performs in-memory parsing only). If the pool is exhausted, workers block waiting for a free connection, reducing effective parallelism.

Size the pool to accommodate concurrent usage:

```
Required connections >= max-workers-for-validator + max-workers-for-committer
```

Setting this too low causes connection starvation — workers sit idle waiting for connections while the database has spare capacity. Setting this too high wastes database server memory and can cause connection-level contention.

The default of 10 is conservative and likely insufficient for production workloads with the default committer worker count of 20. Size this to at least match the total validator + committer worker count.

### `database.min-connections`

Minimum number of idle connections maintained in the pool. Keeps connections warm to avoid the overhead of establishing new connections (TCP handshake + TLS negotiation + authentication) under sudden load spikes. Set to roughly 50% of `max-connections`.

### `database.load-balance`

Enables client-side load balancing across multiple database endpoints. When enabled, each new connection is distributed across the configured endpoints.

- **`true`**: Required for YugabyteDB clusters to distribute operations across nodes and avoid hotspots. Without this, all connections go to the first endpoint, overloading one node while others sit idle.
- **`false`**: Use for single-node deployments.

### `database.table-pre-split-tablets`

Number of tablets to pre-split each table into at creation time (YugabyteDB only). Pre-splitting distributes data across tablet servers from the start, preventing the "hot tablet" bottleneck where a single tablet handles all initial writes before automatic splitting kicks in.

Without pre-splitting, the first hours of operation can see severe write latency spikes as all writes converge on a single tablet. Pre-splitting eliminates this problem entirely.

Set this to match the number of tablet servers in the cluster, or a small multiple (1x-2x). For example, with 9 tablet servers, set to 9 or 18.

When the database is PostgreSQL, this setting is automatically ignored.

| Setting | Effect |
|---------|--------|
| 0 (default) | No pre-splitting; hot tablet during initial writes; latency spikes |
| = tserver count | Even distribution from the start; predictable latency |
| 2x tserver count | Finer distribution; slightly more per-tablet overhead |

### `database.retry`

Exponential backoff retry strategy for database operations. Required for YugabyteDB to handle retryable transaction conflicts (e.g., serialization failures from concurrent MVCC operations).

| Parameter | Default | Effect |
|-----------|---------|--------|
| `initial-interval` | 500ms | First retry delay |
| `randomization-factor` | 0.5 | Jitter range (+/- 50%) to avoid thundering herd |
| `multiplier` | 1.5 | Exponential backoff factor |
| `max-interval` | 60s | Cap on any single retry delay |
| `max-elapsed-time` | 15m | Total retry duration before giving up |

For high-throughput workloads with frequent transaction conflicts, reduce `initial-interval` to 100-200ms and `max-interval` to 10-30s to retry faster. Too-aggressive retries (very short intervals) can amplify contention under heavy load.

## 7. Query Service

### `min-batch-keys`

Minimum number of keys that must accumulate in a query batch before executing against the database. The batch is submitted when this threshold is reached or `max-batch-wait` expires, whichever comes first.

Batching reduces database round-trips by combining keys from multiple concurrent requests into a single query. Setting this too low sends many small queries, increasing per-query overhead. Setting this too high delays queries waiting for the batch to fill, increasing latency for individual requests.

| Setting | Throughput | Latency |
|---------|-----------|---------|
| 256-512 | Lower (smaller batches, more round-trips) | Lower |
| 1024 (default) | Good balance | Moderate |
| 2048-4096 | Higher (larger batches, fewer round-trips) | Higher |

### `max-batch-wait`

Maximum time to wait for a batch to reach `min-batch-keys`. This bounds the worst-case latency during low-load periods when keys accumulate slowly. Setting this too low defeats batching (queries execute before batches can fill). Setting this too high adds latency during quiet periods.

The default of 100ms is appropriate for most deployments. Reduce to 50ms for lower latency.

### `view-aggregation-window`

Time window for aggregating multiple views with the same parameters (isolation level, deferrable mode) into a single batched view. Views created within this window share the same batcher, reducing database load.

Setting this too low creates many independent batchers, each holding its own database connection — increasing connection pool pressure. Setting this too high delays the first query in a window, as new views must wait for the window to open a batcher.

The default of 100ms balances throughput and latency.

### `max-aggregated-views`

Maximum number of views that can be aggregated into a single batcher. Once reached, a new batcher is created even if within the `view-aggregation-window`. This prevents a single batcher from becoming a contention point under very high concurrency. The default of 1024 is appropriate for most deployments.

### `max-active-views`

Maximum concurrent active views across all clients. New `BeginView` requests are rejected with `RESOURCE_EXHAUSTED` when this limit is reached.

Setting this too low causes clients to receive errors during peak load, forcing retries that amplify latency. Setting this too high allows unbounded resource consumption. Set to 0 to disable the limit (not recommended in production). The default of 4096 is permissive.

### `max-view-timeout`

Maximum lifetime of a view from creation to completion. Views exceeding this timeout are aborted and their queries return errors. This prevents dangling views from holding resources indefinitely.

Setting this too low causes legitimate long-running queries to be aborted. Setting this too high allows idle or abandoned views to hold database connections for extended periods, reducing pool availability for other views.

This parameter interacts with the connection pool. The maximum number of database connections needed by the Query Service depends on:

```
max connections needed = (max-view-timeout / view-aggregation-window) × view-parameter-permutations
```

There are up to 8 view parameter permutations (4 isolation levels × 2 deferrable modes). With defaults: `(10s / 100ms) × 8 = 800`. In practice, not all permutations are used simultaneously, so fewer connections are needed. Monitor connection pool wait metrics to determine the right size.

### `max-request-keys`

Maximum number of keys allowed in a single query request. Applies to both `GetRows` (total keys across all namespaces) and `GetTransactionStatus` (number of transaction IDs). Setting this too low forces clients to split queries into many small requests, increasing round-trip overhead. Setting this too high allows individual requests to cause memory spikes and long-running queries that block the connection pool. The default of 10,000 is a reasonable balance.

### `database.max-connections` (Query Service)

The Query Service connection pool operates differently from the VC pool. Connections are held for the duration of a view's queries, which can be up to `max-view-timeout`. A view that holds a connection for 10 seconds blocks that connection from serving other views.

Size the pool based on expected concurrent view load and the formula above. The default of 10 is conservative — increase for production workloads. Monitor the connection pool wait metrics to detect when views are blocking on connection acquisition.

## 8. Database

### YugabyteDB Considerations

- **Tablet distribution**: Use `table-pre-split-tablets` (VC config) to distribute tablets evenly across all tablet servers. Without pre-splitting, initial writes hit a single tablet, causing latency spikes until automatic splitting occurs.
- **Rebalancing**: After adding new tablet servers, rebalance tablets to distribute data to the new nodes.
- **Connection load balancing**: Enable `database.load-balance: true` on all services connected to YugabyteDB to distribute queries across nodes.

### PostgreSQL Considerations

- **`table-pre-split-tablets`**: Automatically ignored when PostgreSQL is detected.
- **Connection pool**: Same `max-connections` and `min-connections` parameters apply. Size based on concurrent query volume.
- **Replication**: For read-heavy workloads, configure Query Service instances to connect to read replicas.

## 9. Co-location Impact

MVCC validation requires multiple database round-trips per transaction — read set validation, write set application, and status updates. When VC instances are co-located with database nodes, each of these round-trips takes microseconds instead of milliseconds. Without co-location, expect significantly higher commit latency, which directly limits overall system throughput.

Co-location is most impactful for the VC service because it performs the most database operations per transaction. The Query Service benefits less because its read-only queries are less latency-sensitive.

## 10. Benchmarking

The tuning recommendations in this guide are starting points, not guarantees. Real performance depends on factors specific to your deployment:

- Transaction size and complexity (number of read/write keys per transaction)
- Number of namespaces and endorsement policy complexity
- Read set and write set sizes
- Network topology and latency between nodes
- Storage hardware characteristics

Benchmark with your actual workload on your target hardware to establish baseline performance and identify the tuning parameters that matter most for your use case.
