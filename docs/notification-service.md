<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Notification Service — Client Usage Guide

The Sidecar exposes a Notification Service that provides two mechanisms for receiving transaction status updates:

1. **Transaction ID Subscription** — Allows clients to subscribe to transaction status
   updates and receive asynchronous notifications when transactions are committed, rejected, or aborted.
   This is the primary mechanism for clients that submit transactions asynchronously to the Ordering
   Service to learn the outcome of their transactions, without polling or scanning the entire block stream.
   It uses a bidirectional gRPC stream: clients send subscription requests
   containing transaction IDs of interest, and the server pushes status responses as transactions
   complete. Multiple subscription requests can be sent on the same stream.

2. **All Transactions Stream** — Allows clients to subscribe to a stream of all committed transactions in block order,
   with optional filtering by namespace and status. This is useful for audit, monitoring, event-driven applications, and
   replication systems.

For internal architecture details, see [sidecar.md — Section 6](sidecar.md#6-notification-service).

## 1. API Definition

The Notification Service provides two streaming RPCs:

From [fabric-x-common/api/committerpb](https://github.com/hyperledger/fabric-x-common)

```protobuf
service Notifier {
    // Subscribe to specific transaction IDs
    rpc OpenNotificationStream (stream NotificationRequest) returns (stream NotificationResponse);

    // Subscribe to all committed transactions
    rpc StreamAllTransactions (StreamAllRequest) returns (stream TxBatch);
}
```

## 2. Transaction ID Subscription API

### 2.1. API Definition

The client sends `NotificationRequest` messages, each containing a batch of transaction IDs to watch
and a timeout:

```protobuf
message NotificationRequest {
    TxIDsBatch tx_status_request = 1;  // List of transaction IDs to subscribe to
    google.protobuf.Duration timeout = 2;  // Timeout for this request
}

message TxIDsBatch {
    repeated string tx_ids = 1;
}
```

The server responds with `NotificationResponse` messages containing status events for
completed transactions, a list of transaction IDs that timed out, or rejected transaction IDs:

```protobuf
message NotificationResponse {
    repeated TxStatus tx_status_events = 1; // List of transaction status events.
    repeated string timeout_tx_ids = 2;     // List of timeout events.
    RejectedTxIds rejected_tx_ids = 3;      // List of rejected transaction IDs with a reason.
}

message TxStatus {
    TxRef ref = 1;
    Status status = 2;
}

message TxRef {
    string tx_id = 1;
    uint64 block_num = 2;
    uint32 tx_num = 3;
}

message RejectedTxIds {
    repeated string tx_ids = 1; // List of rejected transaction IDs.
    string reason = 2;          // The reason for rejection.
}
```

### 2.2. Subscribing to Transaction Status Updates

Create a gRPC connection to the Sidecar, open a notification stream, and send a
`NotificationRequest` with the transaction IDs to watch:

```go
conn, err := grpc.NewClient(sidecarEndpoint, dialOpts...)
client := committerpb.NewNotifierClient(conn)

stream, err := client.OpenNotificationStream(ctx)
if err != nil {
    return err
}

err = stream.Send(&committerpb.NotificationRequest{
    TxStatusRequest: &committerpb.TxIDsBatch{
        TxIds: []string{"txID-1", "txID-2", "txID-3"},
    },
    Timeout: durationpb.New(3 * time.Minute),
})
```

**Timeout semantics:**

- The server enforces an upper bound (`max-timeout`, default: 1 minute, configurable).
  If the client-specified timeout exceeds `max-timeout`, it is capped to `max-timeout`.
  If the client sends zero or a negative value, the server uses `max-timeout`.
- When the timeout expires and some subscribed transaction IDs have not yet completed,
  the server responds with those IDs in the `timeout_tx_ids` field.

Multiple `NotificationRequest` messages can be sent on the same stream. Each request is
tracked independently with its own timeout.

## 2.3. Receiving Notifications

The client receives `NotificationResponse` messages by calling `Recv()` on the stream.
Each response contains one of the following payloads:

1. **`TxStatusEvents`** — A batch of statuses for transactions that have completed.
   Each `TxStatus` includes the transaction ID, block number, transaction index within the
   block, and the final status code. The status code indicates whether the transaction was
   committed (`COMMITTED`), aborted (e.g., `ABORTED_SIGNATURE_INVALID`), or rejected
   during validation (e.g., `REJECTED_DUPLICATE_TX_ID`, `MALFORMED_BAD_ENVELOPE`).

2. **`TimeoutTxIds`** — A list of transaction IDs from a request whose timeout expired
   before the transactions completed.

3. **`RejectedTxIds`** — A list of transaction IDs that were rejected at subscription time,
   along with the reason for rejection (e.g., exceeding the subscription limit).
   The client should not expect status updates for these transaction IDs.

```go
for {
    res, err := stream.Recv()
    if err != nil {
        return err
    }

    if len(res.TxStatusEvents) > 0 {
        for _, txStatus := range res.TxStatusEvents {
            fmt.Printf("TX %s: status=%v block=%d txNum=%d\n",
                txStatus.Ref.TxId,
                txStatus.Status,
                txStatus.Ref.BlockNum,
                txStatus.Ref.TxNum,
            )
        }
    }

    if len(res.TimeoutTxIds) > 0 {
        fmt.Printf("Timed out waiting for: %v\n", res.TimeoutTxIds)
    }

    if res.RejectedTxIds != nil {
        fmt.Printf("Rejected IDs: %v, reason: %s\n",
            res.RejectedTxIds.TxIds,
            res.RejectedTxIds.Reason,
        )
    }
}
```

Responses are batched per stream for efficiency — if multiple subscribed transaction IDs
complete in the same coordinator status update, they are grouped into a single
`NotificationResponse`.

### 2.4. Recommended Client Pattern

To avoid missing notifications, clients should follow this sequence:

1. **Open the notification stream and subscribe** to the transaction IDs of interest.
2. **Then submit the transaction** to the Ordering Service.

This ordering matters because if a transaction completes before the subscription is
registered, no notification will be sent for it.

If the notification stream breaks (e.g., sidecar restart) or the timeout expires before
the transaction completes, the client should fall back to the Block Query API to check
the transaction status.

## 3. All Transactions Stream API

### 3.1. API Definition

The `StreamAllTransactions` RPC provides a server-streaming interface that delivers all committed transactions in block
order. Unlike the transaction ID subscription API, this stream does not require clients to know transaction IDs in
advance.

```protobuf
message StreamAllRequest {
    repeated string filter_namespaces = 1;  // Optional: filter by namespace(s)
    repeated Status filter_status = 2;      // Optional: filter by status
    bool include_read_write_sets = 3;       // Optional: include read/write sets
    bool include_endorsements = 4;          // Optional: include endorsements
}

message TxBatch {
    repeated TxEvent events = 1;
}

message TxEvent {
    TxRef ref = 1;                          // Transaction reference
    Status status = 2;                      // Transaction status
    repeated TxNamespace namespaces = 3;    // Namespaces (if filtering enabled)
    repeated Endorsement endorsements = 4;  // Endorsements (if requested)
}
```

### 3.2. Opening a Stream

Create a gRPC connection to the Sidecar and call `StreamAllTransactions`:

```go
client := committerpb.NewNotifierClient(conn)
stream, err := client.StreamAllTransactions(ctx, &committerpb.StreamAllRequest{
    FilterNamespaces: []string{"namespace-1", "namespace-2"},
    FilterStatus: []committerpb.Status{committerpb.Status_COMMITTED},
    IncludeReadWriteSets: true,
    IncludeEndorsements: false,
})
```

**Request Parameters:**

- **`filter_namespaces`** (optional): If specified, only transactions touching these namespaces are included. Empty
  means all namespaces. Multiple namespaces use OR logic: a transaction is included if it touches any of the specified
  namespaces.

- **`filter_status`** (optional): If specified, only transactions with these status codes are included. Empty means all
  statuses.

- **`include_read_write_sets`** (optional): If true, the `namespaces` field in each `TxEvent`
  includes the read/write sets for each namespace. Default: false.

- **`include_endorsements`** (optional): If true, the `endorsements` field in each `TxEvent`
  includes the transaction endorsements. Default: false.

### 3.3. Receiving Transaction Events

The server sends `TxBatch` messages containing one or more `TxEvent` entries. Transactions are batched by block. All
transactions from the same block are delivered in a single `TxBatch`.

```go
for {
    batch, err := stream.Recv()
    ...

    for _, event := range batch.Events {
        ...
    }
}
```

### 3.4. Stream Behavior

**Block Order Guarantee:**
Transactions are streamed in deterministic block order. All transactions from block N are delivered before any
transactions from block N+1.

**Starting Point:**
The stream starts from the currently processed block when the client connects. Historical transactions are not included.

**No Recovery:**
If the stream disconnects, the client must reconnect and will resume from the current block. Transactions from missed
blocks are not replayed. For guaranteed delivery, use the Block Delivery API instead.

**Backpressure:**
If the client cannot keep up with the transaction rate, the server will block sending to that client. The server uses a
configurable write timeout (default: 30 seconds) to prevent slow clients from blocking the system.

## 4. Concurrency Limits

The Notification Service shares the server's `max-concurrent-streams` limit (default: 10)
with the Block Delivery streams ([Section 5 of sidecar.md](sidecar.md#5-block-delivery-service)).
All stream types compete for the same pool of stream slots.

When the limit is reached, new stream requests are rejected with a gRPC `ResourceExhausted`
status code. Clients should handle this error with appropriate backoff and retry logic.

## 5. Configuration

The following configuration options in `sidecar.yaml` control notification behavior:

| Setting                             | Default | Description                                                                                     |
|-------------------------------------|---------|-------------------------------------------------------------------------------------------------|
| `notification.max-timeout`          | `1m`    | Upper limit on per-request timeout for transaction ID subscriptions.                            |
| `notification.stream-write-timeout` | `30s`   | Write timeout for all transactions stream. Prevents slow clients from blocking.                 |
| `server.max-concurrent-streams`     | `10`    | Maximum concurrent streaming RPCs across all stream types (Deliver + Notification + StreamAll). |

Sample configuration:

```yaml
notification:
  max-timeout: 10m
  stream-write-timeout: 10s
```
