<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Notification Service — Client Usage Guide

The Sidecar exposes a Notification Service that allows clients to subscribe to transaction status
updates and receive asynchronous notifications when transactions are committed, rejected, or aborted.
This is the primary mechanism for clients that submit transactions asynchronously to the Ordering
Service to learn the outcome of their transactions, without polling or scanning the entire block stream.

The Notification Service uses a bidirectional gRPC stream: clients send subscription requests
containing transaction IDs of interest, and the server pushes status responses as transactions
complete. Multiple subscription requests can be sent on the same stream.

For internal architecture details, see [sidecar.md — Section 6](sidecar.md#6-notification-service).

## 1. API Definition

The Notification Service is defined as a bidirectional streaming RPC:

From [fabric-x-common/api/committerpb](https://github.com/hyperledger/fabric-x-common)

```protobuf
service Notifier {
    rpc OpenNotificationStream (stream NotificationRequest) returns (stream NotificationResponse);
}
```

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

## 2. Subscribing to Transaction Status Updates

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

## 3. Receiving Notifications

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

## 4. Recommended Client Pattern

To avoid missing notifications, clients should follow this sequence:

1. **Open the notification stream and subscribe** to the transaction IDs of interest.
2. **Then submit the transaction** to the Ordering Service.

This ordering matters because if a transaction completes before the subscription is
registered, no notification will be sent for it.

If the notification stream breaks (e.g., sidecar restart) or the timeout expires before
the transaction completes, the client should fall back to the Block Query API to check
the transaction status.

## 5. Concurrency Limits

The Notification Service shares the server's `max-concurrent-streams` limit (default: 10)
with the Block Delivery streams ([Section 5 of sidecar.md](sidecar.md#5-block-delivery-service)).
Both stream types compete for the same pool of stream slots.

When the limit is reached, new stream requests are rejected with a gRPC `ResourceExhausted`
status code. Clients should handle this error with appropriate backoff and retry logic.

## 6. Configuration

The following configuration options in `sidecar.yaml` control notification behavior:

| Setting | Default | Description |
|---------|---------|-------------|
| `notification.max-timeout` | `1m` | Upper limit on per-request timeout. Client timeouts are capped to this value. |
| `server.max-concurrent-streams` | `10` | Maximum concurrent streaming RPCs across all stream types (Deliver + Notification). |

Sample configuration:

```yaml
notification:
  max-timeout: 10m
```
