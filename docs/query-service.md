<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Query Service

1. [Overview](#1-overview)
    - [Core Responsibilities](#core-responsibilities)
2. [View Management](#2-view-management)
3. [API Operations](#3-api-operations)
    - [BeginView](#beginview)
    - [EndView](#endview)
    - [GetRows](#getrows)
    - [GetNamespacePolicies](#getnamespacepolicies)
    - [GetConfigTransaction](#getconfigtransaction)
4. [Performance Optimization](#4-performance-optimization)
    - [Connection Pooling](#connection-pooling)
    - [View and Query Batching](#view-and-query-batching)
    - [Batching Strategy](#batching-strategy)
    - [Lock-Free Design](#lock-free-design)
5. [Configuration](#5-configuration)
6. [Implementation](#6-implementation)
    - [Implementation Flow](#implementation-flow)
    - [Lock-Free Design](#lock-free-design)
    - [Error Handling and Recovery](#error-handling-and-recovery)

## 1. Overview

The Query Service is a component of the Fabric-X Committer architecture that provides efficient, consistent read-only access to the state database. It implements a view-based query mechanism that allows clients to retrieve data with specific isolation guarantees while optimizing database access through sophisticated batching techniques.

### Core Responsibilities

The Query Service performs several key functions:

1. **Namespace Access**: Provides access to data across different namespaces.
2. **Configuration Access**: Retrieves system configuration and policy information.
3. **Efficient Data Retrieval**: Optimizes database access by batching multiple queries and views together.
4. **Efficient View Management**: Creates and manages read-only transactions (AKA views) with configurable isolation levels and timeouts.

## 2. View Management

The Query Service implements a view-based query model that allows clients to:

1. Begin a view with specific isolation requirements
2. Execute multiple queries within the context of that view
3. End the view when finished

A views represents a read-only transaction that provide access to the database with configurable isolation levels:

- **Read Uncommitted**: Lowest isolation level, may read uncommitted changes
- **Read Committed**: Only reads committed data, but may see changes during the view
- **Repeatable Read**: Ensures the same data is returned for repeated reads
- **Serializable**: Highest isolation level, ensures transactions are completely isolated

Consistent access is provided only with **Repeatable Read** and **Serializable**.

Views can also be configured as deferred or non-deferred, affecting transaction behavior. In deferred mode, constraint violations and conflicts are checked only at transaction commit time, potentially allowing operations that would otherwise fail immediately. In non-deferred mode, these checks happen at the time of each operation, causing immediate failures when violations occur. This parameter allows applications to balance between early error detection and performance optimization.

## 3. API Operations

The Query Service implements the `QueryServiceServer` interface defined in the Protocol Buffers API:

### BeginView

```go
BeginView(ctx context.Context, params *protoqueryservice.ViewParameters) (*protoqueryservice.View, error)
```

Begins a new view with the specified parameters, including isolation level and timeout. Returns a view identifier that can be used for subsequent queries.
It may create a new read-only database transation, or reuse an existing one.
See [6. Performance Optimization](#6-performance-optimization) below.

### EndView

```go
EndView(ctx context.Context, view *protoqueryservice.View) (*protoqueryservice.View, error)
```

Ends a previously created view, releasing any associated resources.

### GetRows

```go
GetRows(ctx context.Context, query *protoqueryservice.Query) (*protoqueryservice.Rows, error)
```

Retrieves rows from the database based on the specified query parameters. The query includes:

- A view identifier
- A list of namespaces to query
- For each namespace, a list of keys to retrieve

### GetNamespacePolicies

```go
GetNamespacePolicies(ctx context.Context, _ *protoqueryservice.Empty) (*protoblocktx.NamespacePolicies, error)
```

Retrieves the policies associated with all namespaces from the metadata namespace.

### GetConfigTransaction

```go
GetConfigTransaction(ctx context.Context, _ *protoqueryservice.Empty) (*protoblocktx.ConfigTransaction, error)
```

Retrieves the current configuration transaction from the config namespace.

## 4. Performance Optimization

### Connection Pooling

The Query Service uses a connection pool to efficiently manage database connections:

1. **Connection Reuse**: Connections are reused across multiple queries to reduce overhead. 
2. **Connection Limits**: The pool size is configurable to prevent resource exhaustion.
3. **Transaction Management**: Transactions are created and managed as needed.

### View and Query Batching

To optimize database access, the Query Service implements a sophisticated batching mechanism:

1. **View Aggregation**: Multiple views are aggregated into a single read-only database transaction based on the view parameters and timing to reduce number of active database transactions and reduce the number of states the database have to maintain.
2. **Request Aggregation**: Multiple client requests are aggregated into batches based on namespace and timing to reduce number of round-trip to the database and reduce the number of tranactions the database have to manage.
3. **Concurrent Processing**: Batches are processed concurrently to maximize throughput.
4. **Result Distribution**: Results are distributed back to the appropriate clients.

### Batching Strategy

The Query Service employs a sophisticated batching strategy to optimize database access:

1. **Time-Based Batching**: Queries and views are collected over a configurable time window before being executed.
2. **Parameter-Based Grouping**: Queries and views with similar parameters (isolation level, deferred status) are grouped together.
3. **Namespace Separation**: Queries are separated by namespace to allow for parallel processing.

### Lock-Free Design

To minimize contention and maximize throughput, the Query Service uses lock-free data structures:

1. **Concurrent Maps**: For storing view and batch information.
2. **Atomic Operations**: For updating counters and state.
3. **Channel-Based Communication**: For signaling between components.

## 5. Configuration

The Query Service configuration is designed to optimize database access by efficiently batching queries while maintaining performance and resource constraints. The configuration is defined in a YAML file with the following key parameters:

### Server Configuration

- **Server**: Network configuration for the gRPC server, including address and port.

### Database Configuration

- **Database**: Parameters for connecting to the underlying database, including:
   - Connection string
   - Credentials
   - Connection pool settings
   - Timeout settings

### Query Batching Parameters

- **MinBatchKeys**: Minimum number of keys required to trigger a batch query execution. When a batch contains at least this many keys, it becomes eligible for execution.
- **MaxBatchWait**: Maximum time a batch will wait for additional keys before being executed, even if it hasn't reached MinBatchKeys. This ensures queries don't wait indefinitely.

### View Management Parameters

- **ViewAggregationWindow**: Time window during which views with identical parameters will be batched together. Views created within this window that have the same isolation level and deferred status will share database resources.
- **MaxAggregatedViews**: Maximum number of views that can be aggregated together in a single batch. This prevents excessive resource consumption.
- **MaxViewTimeout**: Maximum allowed lifetime for a view. This includes the time required to execute the last query in the view. If a query is executed after this timeout expires, it will be aborted.

### Connection Management

The number of database connections should be carefully configured based on the view parameters. The recommended minimum number of connections can be calculated as:

```
(MaxViewTimeout / ViewAggregationWindow) * <number-of-used-view-configuration-permutations>
```

Where the number of view configuration permutations is determined by the isolation levels and deferred status options in use (maximum of 8 permutations: 4 isolation levels × 2 deferred options).

If there are insufficient database connections available, queries will wait until a connection becomes available, which may exceed the MaxBatchWait time.

A sample configuration structure is defined in the `Config` type within the query package:

```go
type Config struct {
    Server                *connection.ServerConfig
    Database              *vc.DatabaseConfig
    MinBatchKeys          int
    MaxBatchWait          time.Duration
    ViewAggregationWindow time.Duration
    MaxAggregatedViews    int
    MaxViewTimeout        time.Duration
}
```

## 6. Implementation

The Query Service consists of several key components:

* **Service**: The main entry point that implements the gRPC `QueryServiceServer` interface. It handles incoming requests, manages views, and coordinates query execution.
* **ViewsBatcher**: The component that manages views and batches queries for efficient database access. It uses lock-free data structures to minimize contention and maximize throughput.
* **ViewHolder**: Represents a client's view of the database with specific isolation requirements. Each view has a unique ID and parameters that determine its behavior.
* **Batcher**: Aggregates queries with similar parameters to optimize database access. Multiple views with the same parameters may share a batcher.
* **NamespaceQueryBatch**: Collects keys to be queried within a specific namespace. Once a batch reaches a certain size or age, it is executed as a single database query.

### Implementation Flow

The Query Service operates with the following three stages.

#### Stage 1. View Management

View management provides clients with configurable isolation levels to the database.
This stage is optional as the client can call `GetRows()` with `nil` view.
Using `nil` view does not offer any consistency between future calls to `GetRows()`.

**a. View Creation:**

When a client calls `BeginView()`, the service:

1. Validates the requested timeout, capping it at the configured maximum if necessary
2. Generates a unique UUID for the view
3. Creates a new `viewHolder` with the specified parameters
4. Stores the view in the `viewIDToViewHolder` map

```go
func (q *Service) BeginView(
    ctx context.Context, params *protoqueryservice.ViewParameters,
) (*protoqueryservice.View, error) {
	// ...
    // Validate and cap timeout.
    if params.TimeoutMilliseconds == 0 ||
        int64(params.TimeoutMilliseconds) > q.config.MaxViewTimeout.Milliseconds() {
        params.TimeoutMilliseconds = uint64(q.config.MaxViewTimeout.Milliseconds())
    }

    // Generate unique view ID and create view.
	// We try again if we have view-id collision.
    for ctx.Err() == nil {
        viewID, err := getUUID()
        if err != nil { return nil, err }
        if q.batcher.makeView(viewID, params) {
            return &protoqueryservice.View{Id: viewID}, nil
        }
    }
    return nil, ctx.Err()
}
```

**b. View Parameters:**

Each view is configured with specific parameters:

1. **Isolation Level**: Determines the consistency guarantees for the view

    - Read Uncommitted (0): Lowest isolation, may read uncommitted changes
    - Read Committed (1): Only reads committed data
    - Repeatable Read (2): Ensures the same data is returned for repeated reads
    - Serializable (3): (default) Highest isolation, ensures transactions are completely isolated

2. **Deferred Status**: Controls when isolation level violations are checked

    - Non-deferred (false): Checks happen immediately for each operation
    - Deferred (true): (default) Checks happen only at transaction commit time

3. **Timeout**: Maximum lifetime for the view
 
    - If omitted, it defaults to the maximum value defined in the service configuration

**c. View Aggregation:**

To optimize resource usage, views with identical parameters created within the `ViewAggregationWindow` are batched together:

1. The service calculates a parameter key based on the isolation level and deferred status
2. It checks if a batcher already exists for these parameters
3. If a batcher exists and is within the aggregation window, the view is associated with it
4. Otherwise, a new batcher is created

**d. View Termination:**

When a client calls `EndView()` or when a view times out:

1. The view is removed from the `viewIDToViewHolder` map
2. If the view was the last reference to its batcher, the batcher is cleaned up
3. Any resources associated with the view are released

#### Stage 2. Query Batching and Execution

Query batching is an optimization that allows the service to minimize database access by combining multiple queries into a single operation.

**a. Query Assignment:**

When a client calls `GetRows()`, the service:

1. Retrieves the view associated with the provided view ID
2. For each namespace in the query:
    - Gets or creates a `namespaceQueryBatch` for that namespace
    - Adds the requested keys to the batch
    - Returns a reference to the batch to the client

```go
func (q *Service) assignRequest(
    ctx context.Context, query *protoqueryservice.Query,
) ([]*namespaceQueryBatch, error) {
	// ...
    batcher, err := q.batcher.getBatcher(ctx, query.View)
    if err != nil { return nil, err }

    batches := make([]*namespaceQueryBatch, len(query.Namespaces))
    for i, ns := range query.Namespaces {
        batches[i], err = batcher.addNamespaceKeys(ctx, ns.NsId, ns.Keys)
        if err != nil { return nil, err }
    }
    return batches, nil
}
```

**b. Batch Execution Triggers:**

A batch is executed when either of these conditions is met:

1. The batch reaches `MinBatchKeys` keys
2. The batch has been waiting for `MaxBatchWait` duration

**c. Database Query Execution:**

When a batch is ready for execution:

1. The batcher acquires a database connection from the pool
2. It constructs a SQL query that retrieves all keys in the batch in a single operation
3. The query is executed with the appropriate isolation level
4. Results are stored in the batch for distribution to clients

**d. Concurrent Batch Management:**

Multiple batches can be in different stages simultaneously:

1. Some batches may be collecting keys
2. Others may be waiting to reach the minimum batch size
3. Some may be executing database queries
4. Others may be distributing results to clients

The service uses lock-free data structures to manage this concurrency efficiently.

#### Stage 3. Result Distribution

Once a batch query completes, the results need to be distributed to the clients that requested them.

**a. Result Storage:**

When a batch query completes:

1. The results are parsed from the database response
2. They are stored in a map keyed by the key bytes
3. The batch is marked as finalized

**b. Client Notification:**

Clients waiting for results are notified through a combination of:

1. Mutex-protected state changes
2. Context cancellation for error cases
3. Direct result access for successful queries

**c. Result Retrieval:**

When `waitForRows()` is called on a batch:

1. If the batch is already finalized, results are returned immediately
2. Otherwise, the client waits until the batch is finalized or the context is canceled
3. Once finalized, the client extracts only the results for the keys it requested

```go
func (b *namespaceQueryBatch) waitForRows(ctx context.Context, keys [][]byte) ([]*protoqueryservice.Row, error) {
    // Wait for batch to be finalized or context to be canceled.
    select {
    case <-ctx.Done():
        return nil, ctx.Err()
    case <-b.ctx.Done():
        // Query completed.
    }
	
	// ...

    // Extract results for requested keys
    res := make([]*protoqueryservice.Row, len(keys))
    for i, key := range keys {
        // Get result for this key from the batch results.
		if row, ok := q.result[string(key)]; ok && row != nil {
			res = append(res, row)
		}
    }
    return rows, nil
}
```

### Lock-Free Design

The Query Service uses lock-free data structures to minimize contention and maximize throughput.

**a. Concurrent Maps:**

The service uses specialized concurrent maps for:

1. Mapping view IDs to view holders
2. Mapping view parameters to batchers
3. Mapping namespaces to query batches

These maps allow multiple goroutines to access and modify the maps concurrently without blocking.

**b. Update-or-Create Pattern:**

A key pattern used throughout the service is the `mapUpdateOrCreate` function, which:

1. Attempts to load an existing value from a map
2. If found, tries to update it using a provided function
3. If not found or update fails, creates a new value
4. Uses atomic compare-and-swap operations to ensure consistency

```go
func mapUpdateOrCreate[K, V any](
    ctx context.Context, m *utils.SyncMap[K, *V], key K, methods updateOrCreate[V],
) (*V, error) {
    val, loaded := m.Load(key)
    for ctx.Err() == nil {
        // If there is a value, and we can update it, then return it.
        if loaded && val != nil {
            if methods.update(val) {
                return val, nil
            }
        }

        // Otherwise, let's try to assign a new value.
        newVal := methods.create()
        var assigned bool
        if !loaded {
            val, loaded = m.LoadOrStore(key, newVal)
            assigned = !loaded
        } else if assigned = m.CompareAndSwap(key, val, newVal); !assigned {
            // If the CAS failed, we need to load the new value.
            val, loaded = m.Load(key)
        }

        if assigned {
            methods.post(newVal)
            val = newVal
            loaded = true
        }
    }
    return nil, ctx.Err()
}
```

**c. Minimal Locking:**

When locks are necessary, they are used with minimal scope:

1. Fine-grained locks protect only the specific data being modified
2. Operations that don't modify shared state are performed outside lock sections
3. Long-running operations like database queries are performed without holding locks

### Error Handling and Recovery

The Query Service is designed to handle various error conditions gracefully.

**a. View Timeout Handling:**

Each view has a context with a timeout based on the client's request:

1. When a view times out, its context is canceled
2. All operations associated with the view receive context cancellation errors
3. Resources associated with the view are cleaned up

**b. Database Connection Failures:**

If a database connection fails:

1. The affected batch is marked as failed
2. Clients waiting for results receive an error
3. The service continues processing other batches
4. New connection attempts are made for subsequent operations

**c. Invalid View Handling:**

If a client attempts to use an invalid or expired view:

1. The service returns `ErrInvalidOrStaleView`
2. The client can create a new view and retry the operation

**d. Graceful Shutdown:**

When the service is shutting down:

1. The main context is canceled
2. All view contexts are canceled
3. In-flight operations are allowed to complete or time out
4. Database connections are properly closed

This comprehensive error handling ensures the service remains stable and recovers gracefully from failures.
