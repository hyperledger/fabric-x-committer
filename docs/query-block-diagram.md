<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Query Service - Block Diagram

This document provides a detailed block diagram of the query service components and how data flows between them.

## High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────────────
│                           QUERY SERVICE
│
│                        ┌────────────────┐
│                        │  Client Apps   │
│                        │  (External)    │
│                        └────────┬───────┘
│                                 │
│                                 │ gRPC Requests (QueryService):
│                                 │ • BeginView(ViewParameters) → View
│                                 │ • EndView(View) → Empty
│                                 │ • GetRows(Query) → Rows
│                                 │ • GetTransactionStatus(TxStatusQuery) → TxStatusResponse
│                                 │ • GetNamespacePolicies(Empty) → NamespacePolicies
│                                 │ • GetConfigTransaction(Empty) → ConfigTransaction
│                                 │
│                    ┌────────────┼────────────┐
│                    │            │            │
│                    ▼            ▼            ▼
│         ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│         │  Query       │  │  Query       │  │  Query       │
│         │  Service     │  │  Service     │  │  Service     │
│         │  Instance 1  │  │  Instance 2  │  │  Instance N  │
│         └──────┬───────┘  └──────┬───────┘  └──────┬───────┘
│                │                 │                 │
│                │                 │                 │
│                └─────────────────┼─────────────────┘
│                                  │
│                                  │ SQL Queries
│                                  │
│                    ┌─────────────▼─────────────┐
│                    │  PostgreSQL/YugabyteDB   │
│                    │  (State Database)        │
│                    │  - Namespace tables      │
│                    │  - tx_status table       │
│                    │  - Config/Policy tables  │
│                    └──────────────────────────┘
│
└─────────────────────────────────────────────────────────────────────────────
```

## View Lifecycle and Aggregation

Understanding view lifecycle and aggregation is key to understanding how the query service achieves high performance through batching.

### Client Workflow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         CLIENT WORKFLOW                                     │
└─────────────────────────────────────────────────────────────────────────────┘

Typical Client Interaction:
  1. BeginView(ViewParameters) → View{Id: "uuid-123"}
     └─► Creates a consistent view with specified isolation level

  2. GetRows(View, Query) → Rows
     └─► Query data within the view (repeatable)

  3. GetRows(View, Query) → Rows
     └─► Additional queries in same view see consistent snapshot

  4. EndView(View) → Empty
     └─► Release view resources

Alternative: Non-Consistent Queries
  - GetRows(nil, Query) → Rows
    └─► Query without view (each query gets independent snapshot)
```


### View Lifecycle Management

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                      VIEW LIFECYCLE MANAGEMENT                              │
└─────────────────────────────────────────────────────────────────────────────┘

View Creation (BeginView):
  1. Generate unique view ID (UUID)
  2. Check MaxActiveViews limit (semaphore)
  3. Create viewHolder with timeout context
  4. Store in viewIDToViewHolder map
  5. Setup automatic cleanup on timeout

View Assignment to Batcher (First Query):
  1. Compute batcher key from view parameters
  2. Try to join existing batcher (lock-free)
     - If exists and not full: join (increment refCounter)
     - If doesn't exist or full: create new batcher
  3. Assign batcher to viewHolder
  4. Setup view cancellation when batcher ends

View Termination (EndView or Timeout):
  1. Remove from viewIDToViewHolder map
  2. Cancel view context
  3. Release view limiter semaphore
  4. Call batcher.leave()
     - Decrement refCounter
     - If refCounter == 0: cancel batcher context
       → Rollback transaction
       → Cleanup all namespace batches

Automatic Cleanup:
  - View timeout: context.AfterFunc(viewTimeout, cleanup)
  - Batcher aggregation window: time.AfterFunc(window, remove from map)
  - Batch timeout: time.AfterFunc(MaxBatchWait, submit)
```

### View Aggregation Strategy

The query service uses a sophisticated aggregation strategy to maximize database efficiency:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    VIEW AGGREGATION ARCHITECTURE                            │
└─────────────────────────────────────────────────────────────────────────────┘

Key a, b

a:1, b:1

C V: abc
R a: 1

W a:2, b:2
R b: 1

Key Concept: Multiple views with identical parameters share a single database
transaction through a "batcher" component.

View Parameters → Batcher Key:
  - IsoLevel: SERIALIZABLE, REPEATABLE_READ, READ_COMMITTED, READ_UNCOMMITTED
  - NonDeferrable: true or false
  - Result: 8 possible batcher permutations (4 iso levels × 2 defer modes)

Aggregation Rules:
  1. Views with SAME parameters created within ViewAggregationWindow
     → Assigned to SAME batcher
  2. Each batcher can serve up to MaxAggregatedViews views
  3. All queries from aggregated views use the SAME database transaction
  4. After ViewAggregationWindow expires, new views create new batcher

Timeline Example:

Time: 0ms
  Client A: BeginView(SERIALIZABLE, DEFERRABLE)
    └─► Creates View1 (viewID="uuid-1")
        ├─► viewIDToViewHolder["uuid-1"] = View1
        └─► On first query: Creates Batcher1 with sharedLazyTx
            ├─► View1.batcher = Batcher1
            └─► viewParametersToLatestBatcher[key=0] = Batcher1

Time: 50ms (within ViewAggregationWindow = 100ms)
  Client B: BeginView(SERIALIZABLE, DEFERRABLE)
    └─► Creates View2 (viewID="uuid-2")
        ├─► viewIDToViewHolder["uuid-2"] = View2
        └─► On first query: Joins existing Batcher1 (same parameters!)
            ├─► View2.batcher = Batcher1
            └─► Batcher1.refCounter = 2

Time: 75ms
  Client C: BeginView(SERIALIZABLE, DEFERRABLE)
    └─► Creates View3 (viewID="uuid-3")
        ├─► viewIDToViewHolder["uuid-3"] = View3
        └─► On first query: Joins existing Batcher1
            ├─► View3.batcher = Batcher1
            └─► Batcher1.refCounter = 3

Time: 150ms (after ViewAggregationWindow)
  Client D: BeginView(SERIALIZABLE, DEFERRABLE)
    └─► Creates View4 (viewID="uuid-4")
        ├─► viewIDToViewHolder["uuid-4"] = View4
        └─► On first query: Creates Batcher2 (new aggregation window)
            ├─► View4.batcher = Batcher2
            └─► viewParametersToLatestBatcher[key=0] = Batcher2
                (Note: View1, View2, View3 still reference Batcher1!)

Time: 200ms
  Client E: BeginView(REPEATABLE_READ, DEFERRABLE)
    └─► Creates View5 (viewID="uuid-5")
        ├─► viewIDToViewHolder["uuid-5"] = View5
        └─► On first query: Creates Batcher3 (different parameters!)
            ├─► View5.batcher = Batcher3
            └─► viewParametersToLatestBatcher[key=1] = Batcher3

Result:
  - viewIDToViewHolder: Maps each view ID to its viewHolder
    • "uuid-1" → View1 (batcher = Batcher1)
    • "uuid-2" → View2 (batcher = Batcher1)
    • "uuid-3" → View3 (batcher = Batcher1)
    • "uuid-4" → View4 (batcher = Batcher2)
    • "uuid-5" → View5 (batcher = Batcher3)

  - viewParametersToLatestBatcher: Maps parameters to latest batcher
    • key=0 → Batcher2 (but Batcher1 still serves View1, View2, View3!)
    • key=1 → Batcher3

  - Active Batchers:
    • Batcher1: serves View1, View2, View3 (same DB transaction)
    • Batcher2: serves View4 (new DB transaction)
    • Batcher3: serves View5 (different isolation level)

Key Insight:
  Views maintain their batcher reference even after viewParametersToLatestBatcher
  is updated. This allows existing views to continue using their assigned batcher
  while new views get assigned to a new batcher.
```

### Batcher Operation

Each batcher manages query batching for its aggregated views:

```
┌───────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                           BATCHER OPERATION                                                   │
└───────────────────────────────────────────────────────────────────────────────────────────────────────────────┘

Batcher Components:
  - sharedLazyTx: Database transaction (created lazily on first query)
  - nsToLatestQueryBatch: Map of namespace → current query batch
  - refCounter: Number of views using this batcher
  - ctx: Context (cancelled when all views leave OR when any view times out)

Query Flow Through Batcher:

  Client A (View1): GetRows(ns="users", keys=[k1, k2])
  Client B (View2): GetRows(ns="users", keys=[k3, k4])
  Client C (View3): GetRows(ns="orders", keys=[o1])
       │                      │                      │
       │                      │                      │ All views use same batcher
       │                      │                      │
       ▼                      ▼                      ▼
  ┌─────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
  │  Batcher1                                                                                                   │
  │  ┌────────────────────────────────────────────────────────────────────────────────────────────────────────┐ │
  │  │  nsToLatestQueryBatch:                                                                                 │ │
  │  │  - "users" → namespaceQueryBatch1                                                                      │ │
  │  │    • keys: [k1, k2, k3, k4]                                                                            │ │
  │  │    • Waiting for: MinBatchKeys or timeout                                                              │ │
  │  │  - "orders" → namespaceQueryBatch2                                                                     │ │
  │  │    • keys: [o1]                                                                                        │ │
  │  │    • Waiting for: MinBatchKeys or timeout                                                              │ │
  │  └────────────────────────────────────────────────────────────────────────────────────────────────────────┘ │
  │                                                                                                             │
  │  sharedLazyTx (created on first query):                                                                     │
  │  - Transaction: BEGIN READ ONLY SERIALIZABLE DEFERRABLE                                                     │
  │  - All queries use this transaction                                                                         │
  │  - Provides consistent snapshot across all aggregated views                                                 │
  │  - Automatically rolled back when batcher context is cancelled                                              │
  └─────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
       │                                                  │
       │ When batch ready (size or timeout)               │
       │                                                  │
       ▼                                                  ▼
  ┌────────────────────────────────────┐    ┌────────────────────────────────────┐
  │  Execute SQL for "users":          │    │  Execute SQL for "orders":         │
  │                                    │    │                                    │
  │  SELECT key, value, version        │    │  SELECT key, value, version        │
  │  FROM ns_users                     │    │  FROM ns_orders                    │
  │  WHERE key = ANY($1)               │    │  WHERE key = ANY($1)               │
  │                                    │    │                                    │
  │  Parameters:                       │    │  Parameters:                       │
  │  $1 = [k1, k2, k3, k4]             │    │  $1 = [o1]                         │
  └────────────────────────────────────┘    └────────────────────────────────────┘
       │                                                  │
       │ Results stored in batch.result map               │
       │                                                  │
       ▼                                                  ▼
  Client A gets [k1, k2] from batch results
  Client B gets [k3, k4] from batch results
  Client C gets [o1] from batch results

Context Cancellation Triggers:
  1. All views call EndView() → refCounter reaches 0 → batcher.cancel()
  2. Any view times out → view.cancel() → triggers batcher.cancel() (via context.AfterFunc)
  3. ViewAggregationWindow expires → batcher removed from viewParametersToLatestBatcher (but continues serving existing views)

Benefits:
  - Single transaction for multiple views (consistency)
  - Batched queries reduce DB round trips
  - Key deduplication across clients
  - Clients wait on shared batch (efficient)
  - Automatic cleanup on view timeout or explicit termination
```

## Detailed Component Diagram with Data Flow

Now that we understand view aggregation and query batching, here's the high-level component flow:

```
                    ┌───────────────────┐
                    │   Client Apps     │
                    └─────────┬─────────┘
                              │
                              │ gRPC Requests
                              │
┌─────────────────────────────┼─────────────────────────────────────────────────
│ QUERY SERVICE               │
│                             │
│  ┌──────────────────────────▼──────────────────────────────────────────────┐
│  │  QUERY SERVICE (query_service.go)                                       │
│  │                                                                         │
│  │  • BeginView: Create view, assign to batcher (lazy)                     │
│  │  • GetRows: Validate, assign to batch, wait for results                 │
│  │  • GetTransactionStatus: Convert to internal query, process             │
│  │  • EndView: Remove view, trigger batcher.leave()                        │
│  │  • GetNamespacePolicies: Direct query to ns_meta table                  │
│  │  • GetConfigTransaction: Direct query to ns_config table                │
│  └─────────────────────────────────────────────────────────────────────────┘
│                                  │
│                                  │
│  ┌───────────────────────────────▼─────────────────────────────────────────┐
│  │  VIEWS BATCHER (batcher.go)                                             │
│  │                                                                         │
│  │  • viewIDToViewHolder: Map view ID → viewHolder                         │
│  │  • viewParametersToLatestBatcher: Map parameters → batcher              │
│  │  • nonConsistentBatcher: For queries without views                      │
│  │                                                                         │
│  │  Operations:                                                            │
│  │  • makeView: Create viewHolder, check limits, setup cleanup             │
│  │  • getBatcher: Assign view to batcher (create or join existing)         │
│  │  • removeViewID: Cleanup view, trigger batcher.leave()                  │
│  └─────────────────────────────────────────────────────────────────────────┘
│                                  │
│                                  │
│  ┌───────────────────────────────▼─────────────────────────────────────────┐
│  │  BATCHER (batcher.go)                                                   │
│  │                                                                         │
│  │  • nsToLatestQueryBatch: Map namespace → namespaceQueryBatch            │
│  │  • queryObj: sharedLazyTx (consistent) or sharedPool (non-consistent)   │
│  │  • refCounter: Number of views using this batcher                       │
│  │                                                                         │
│  │  Operations:                                                            │
│  │  • join: Assign view to batcher, increment refCounter                   │
│  │  • leave: Decrement refCounter, cancel if zero                          │
│  │  • addNamespaceKeys: Add keys to batch (create or update)               │
│  └─────────────────────────────────────────────────────────────────────────┘
│                                  │
│                                  │
│  ┌───────────────────────────────▼─────────────────────────────────────────┐
│  │  NAMESPACE QUERY BATCH (batcher.go)                                     │
│  │                                                                         │
│  │  • keys: Accumulated keys to query                                      │
│  │  • result: Query results (map[key] → Row)                               │
│  │  • finalized: Prevents new keys after execution starts                  │
│  │                                                                         │
│  │  Operations:                                                            │
│  │  • addKeys: Append keys, submit if ready                                │
│  │  • submit: Launch execute() in goroutine (sync.Once)                    │
│  │  • execute: Acquire connection, deduplicate, query DB, store results    │
│  │  • waitForRows: Wait for completion, extract requested keys             │
│  └─────────────────────────────────────────────────────────────────────────┘
│                                  │
│                                  │
│  ┌───────────────────────────────▼─────────────────────────────────────────┐
│  │  DATABASE QUERIERS (query.go)                                           │
│  │                                                                         │
│  │  sharedPool (non-consistent queries):                                   │
│  │  • Returns new connection from pool each time                           │
│  │  • Each query gets independent snapshot                                 │
│  │                                                                         │
│  │  sharedLazyTx (consistent views):                                       │
│  │  • Creates transaction lazily on first query                            │
│  │  • All queries in batcher use same transaction                          │
│  │  • Provides consistent snapshot across queries                          │
│  │  • Transaction parameters from ViewParameters                           │
│  └─────────────────────────────────────────────────────────────────────────┘
│                                  │
│                                  │ SQL Queries
│                                  │
└──────────────────────────────────┼──────────────────────────────────────────
                                   │
                     ┌─────────────▼─────────────┐
                     │  PostgreSQL/YugabyteDB    │
                     │                           │
                     │  Tables:                  │
                     │  - ns_0, ns_1, ... (data) │
                     │  - ns_meta (policies)     │
                     │  - ns_config (config)     │
                     │  - tx_status (statuses)   │
                     └───────────────────────────┘
```

## Lock-Free Coordination

The query service uses lock-free patterns for high concurrency:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    LOCK-FREE COORDINATION (lock_free.go)                    │
└─────────────────────────────────────────────────────────────────────────────┘

mapUpdateOrCreate Pattern:
  Used for: Batcher assignment, batch creation

  Algorithm:
    Loop until success or context cancelled:
      1. Try to load existing value
      2. If exists: try methods.update(value)
         - If update succeeds: return value
      3. If not exists or update fails:
         - Create new value: methods.create()
         - Try LoadOrStore or CompareAndSwap
         - If successful:
           • Call methods.post(value) (setup resources)
           • Return value
         - If failed: retry loop

  Benefits:
    - No global locks
    - Multiple goroutines progress concurrently
    - At least one goroutine always makes progress
    - Lazy resource allocation (post method)
    - Prevents resource leaks on failed assignments

  Example: Batcher Assignment
    create: () => new batcher with context
    update: (batcher) => batcher.join(view)
    post: (batcher) => setup cleanup timers

  Example: Batch Creation
    create: () => new namespaceQueryBatch
    update: (batch) => batch.addKeys(keys)
    post: (batch) => setup timeout timer
```

## Concurrency Model

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        CONCURRENCY ARCHITECTURE                             │
└─────────────────────────────────────────────────────────────────────────────┘

Lock-Free Structures:
  - viewIDToViewHolder: SyncMap (concurrent view lookups)
  - viewParametersToLatestBatcher: SyncMap (concurrent batcher assignment)
  - nsToLatestQueryBatch: SyncMap (concurrent batch creation)

Synchronization Points:
  - viewHolder.m: Mutex (protects batcher assignment)
  - batcher.m: Mutex (protects refCounter and join/leave)
  - namespaceQueryBatch.m: Mutex (protects keys append and finalization)
  - sharedLazyTx.m: Mutex (serializes transaction access)

Goroutine Model:

  Per gRPC Request:
    - Runs in gRPC server goroutine
    - Adds keys to batch (lock-free)
    - Waits on batch completion (select on context)

  Per Batch:
    - submit() launches execute() in new goroutine
    - execute() runs once (sync.Once)
    - Multiple clients wait on same batch

  Background Tasks:
    - View timeout cleanup (context.AfterFunc)
    - Batcher aggregation window cleanup (time.AfterFunc)
    - Batch timeout (time.AfterFunc)
    - TLS refresh (dedicated goroutine)

Context Hierarchy:
  Service Context
    └─► Batcher Context (cancelled when all views leave)
          └─► View Context (timeout from ViewParameters)
                └─► Batch Context (cancelled on completion or error)
                      └─► Query Context (from gRPC request)
```

## Metrics and Monitoring

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              METRICS                                        │
└─────────────────────────────────────────────────────────────────────────────┘

Request Metrics:
  - queryservice_grpc_requests_total{method}
    └─► Counter: Total requests by method (begin_view, get_rows, etc.)

  - queryservice_grpc_requests_latency_seconds{method}
    └─► Histogram: Request latency by method

  - queryservice_grpc_key_requested_total
    └─► Counter: Total keys requested across all queries

  - queryservice_grpc_key_responded_total
    └─► Counter: Total keys returned in responses

Database Metrics:
  - queryservice_database_processing_sessions{session}
    └─► Gauge: Active sessions by type
        • active_views: Number of active views
        • processing_queries: Queries being processed
        • waiting_queries: Queries waiting for DB connection
        • in_execution_queries: Queries executing in DB
        • transactions: Active database transactions

  - queryservice_database_batch_queueing_time_seconds
    └─► Histogram: Time batches wait before execution

  - queryservice_database_batch_query_size
    └─► Histogram: Number of keys in executed batches

  - queryservice_database_batch_response_size
    └─► Histogram: Number of rows returned by batches

  - queryservice_database_request_assignment_latency_seconds
    └─► Histogram: Time to assign request to batch

  - queryservice_database_query_latency_seconds
    └─► Histogram: Database query execution time

Health Check:
  - gRPC health service (grpc_health_v1.Health)
    └─► Reports service health status
```

## Configuration Parameters

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         CONFIGURATION                                       │
└─────────────────────────────────────────────────────────────────────────────┘

Server Configuration:
  - Server.Port: gRPC server port (default: 7001)
  - Server.TLS: TLS configuration for secure connections
  - Monitoring.Port: Prometheus metrics port (default: 2117)

Database Configuration:
  - Database.Host: PostgreSQL/YugabyteDB host
  - Database.Port: Database port
  - Database.MaxConnections: Connection pool size
    └─► Should be >= (MaxViewTimeout / ViewAggregationWindow) × 8

Batching Configuration:
  - MinBatchKeys: Minimum keys to trigger batch (default: 1024)
    └─► Higher values: Better batching, higher latency
    └─► Lower values: Lower latency, more DB queries

  - MaxBatchWait: Maximum wait time (default: 100ms)
    └─► Higher values: Better batching, higher latency
    └─► Lower values: Lower latency, less efficient batching

View Aggregation Configuration:
  - ViewAggregationWindow: Window to aggregate views (default: 100ms)
    └─► Views with same parameters within this window share a batcher
    └─► Higher values: More aggregation, fewer transactions
    └─► Lower values: Less aggregation, more transactions

  - MaxAggregatedViews: Max views per batcher (default: 1024)
    └─► Limits number of views sharing a transaction
    └─► Higher values: More sharing, potential contention
    └─► Lower values: Less sharing, more transactions

  - MaxActiveViews: Max concurrent views (default: 4096)
    └─► Total limit across all batchers
    └─► Set to 0 for unlimited
    └─► Prevents resource exhaustion

  - MaxViewTimeout: Maximum view lifetime (default: 10s)
    └─► Caps client-specified timeout
    └─► Prevents long-running transactions

Request Limits:
  - MaxRequestKeys: Max keys per request (default: 10000)
    └─► Applies to GetRows and GetTransactionStatus
    └─► Set to 0 for unlimited
    └─► Prevents oversized queries

TLS Configuration:
  - TLSRefreshInterval: Config polling interval (default: 1 minute)
    └─► How often to check for updated CA certificates
```

## Key Design Decisions

### 1. Two-Level Batching
Queries are batched at two levels for maximum efficiency:
- **View Level**: Multiple views share a database transaction (batcher)
- **Namespace Level**: Multiple queries share a SQL query (namespaceQueryBatch)

This provides both consistency (transaction) and efficiency (batching).

### 2. Lock-Free Batching
Uses lock-free data structures with optimistic concurrency control:
- Multiple clients can add keys to batches concurrently
- No global locks for batch creation or view assignment
- Guarantees at least one goroutine makes progress

### 3. View Aggregation
Views with identical parameters share database transactions:
- Reduces transaction overhead
- Provides consistent snapshots across multiple clients
- Limited by `MaxAggregatedViews` to prevent contention

### 4. Lazy Transaction Creation
Database transactions are created lazily on first query:
- Avoids transaction overhead for unused views
- `sharedLazyTx` creates transaction on first `Acquire()`
- Automatic rollback on batcher context cancellation

### 5. Consistent vs Non-Consistent Queries
Two query modes with different guarantees:
- **With View**: Uses `sharedLazyTx`, provides consistent snapshot
- **Without View**: Uses `sharedPool`, each query gets new connection

### 6. Batch Submission Triggers
Batches are submitted when either condition is met:
- **Size Trigger**: `len(keys) >= MinBatchKeys`
- **Time Trigger**: `MaxBatchWait` timeout expires

### 7. Context-Based Lifecycle
All components use context for lifecycle management:
- Views have timeout contexts from `ViewParameters`
- Batchers cancelled when all views leave
- Automatic cleanup via `context.AfterFunc`

### 8. Deduplication
Keys are deduplicated at batch execution time:
- Multiple clients may request same keys
- Single SQL query fetches each unique key once
- Results shared among all waiting clients

### 9. Error Isolation
Errors are isolated to prevent cascading failures:
- Batch errors don't affect other batches
- View errors don't affect other views
- Each component has independent error handling

### 10. Connection Pool Management
Database connections are managed carefully:
- Batches wait for available connections (backpressure)
- Pool size should accommodate concurrent batchers
- Formula: `MaxConnections >= (MaxViewTimeout / ViewAggregationWindow) × 8`

## Benefits

1. **High Throughput**: Two-level batching reduces database round trips
2. **Low Latency**: Configurable timeouts balance batching with responsiveness
3. **Consistency**: Views provide repeatable read semantics
4. **Scalability**: Lock-free design enables high concurrency
5. **Efficiency**: View aggregation and key deduplication minimize database load
6. **Flexibility**: Supports both consistent and non-consistent query modes
7. **Resilience**: Automatic cleanup and error isolation prevent resource leaks
8. **Observability**: Comprehensive metrics for monitoring and tuning
