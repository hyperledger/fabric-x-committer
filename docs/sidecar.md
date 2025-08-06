<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Sidecar Middleware

1. [Overview](#1-overview)
2. [Core Responsibilities](#2-core-responsibilities)
3. [Configuration](#3-configuration)
4. [Workflow Details](#4-workflow-details)
   - [Task 1. Fetching Blocks from the Ordering Service](#task-1-fetching-blocks-from-the-ordering-service)
   - [Task 2. Relaying Blocks to the Coordinator](#task-2-relaying-blocks-to-the-coordinator)
   - [Task 3. Persisting Committed Block in the File System](#task-3-persisting-committed-block-in-the-file-system)
5. [Delivering Committed Block to Registered Clients](#5-delivering-committed-block-to-registered-clients)
6. [Failure and Recovery](#6-failure-and-recovery)
   - [A. State Persistence](#a-state-persistence)
   - [B. Restart and Recovery Procedure](#b-restart-and-recovery-procedure)


## 1. Overview

The Sidecar is a middleware component designed to operate between an Ordering Service and the Coordinator component.
Its primary function is to reliably manage the flow of blocks, ensuring they are fetched, validated, persisted, and
delivered to downstream clients.

## 2. Core Responsibilities

The Sidecar performs four main tasks:

1.  **Fetch Blocks:** Retrieves blocks sequentially from the Ordering Service.
2.  **Relay and Validate:** Forwards fetched blocks to the Coordinator and receives feedback on transaction statuses
    within those blocks.
3.  **Persist Committed Blocks:** Stores blocks confirmed as committed by the Coordinator in a local, append-only file
    store for durability and auditability.
4.  **Deliver to Clients:** Delivers the committed blocks to registered client applications.

Note that the fourth task is executed only when users/clients creates a stream with the sidecar.

## 3. Configuration

The Sidecar requires the following configuration settings, provided in a Yaml configuration
file:

 - *Ordering Service Endpoints*: Address(es) of the ordering service node(s).
 - *Coordinator Endpoint*: Address of the coordinator service.
 - *Listen Address*: The local address and port (e.g., 0.0.0.0:7055) where the Sidecar 
   will host its block delivery service, enabling clients to connect and receive 
   committed blocks.

*Ordering Service Endpoint Precedence*: While ordering service endpoints can be 
specified in Yaml configuration file, they can be overridden by the endpoints defined within 
the channel's configuration block.

A sample `sidecar.yaml` is located at `cmd/config/sample`. 

## 4. Workflow Details

We run three long-running tasks:

*Task 1*: Fetch blocks from the ordering service.
*Task 2*: Relay blocks to the coordinator and receive statuses.
*Task 3*: Commit blocks to the file system.

We use two queues/channels to enable communication among these three tasks:

1. `blocksToBeCommitted` - Task 1 is the producer, while Task 2 is the consumer.
2. `committedBlocks` - Task 2 is the producer, while Task 3 is the consumer. (See note below)

In this section, we provide an overview of each of these tasks in detail.

### Task 1. Fetching Blocks from the Ordering Service

The following code runs *Task 1* which fetches blocks from the ordering service and
enqueue to `blockToBeCommitted` queues.

From [service/sidecar/sidecar.go](/service/sidecar/sidecar.go#L138)

```go
    g.Go(func() error {
      logger.Info("Fetch blocks from the ordering service and write them on s.blockToBeCommitted.")
      err := s.ordererClient.Deliver(gCtx, &broadcastdeliver.DeliverConfig{
        StartBlkNum: int64(nextBlockNum),
        EndBlkNum:   broadcastdeliver.MaxBlockNum,
        OutputBlock: s.blockToBeCommitted,
      })
      if errors.Is(err, context.Canceled) {
        return errors.Wrap(err, "context is canceled")
      }
      return errors.Join(connection.ErrNonRetryable, err)
    })

```

Next, we explain the code internals.

**a. Creating An Orderer Client:**

The Sidecar creates a gRPC client connection (`grpc.ClientConnInterface`) to the Orderer 
using the IP addresses provided in the configuration. It creates an `AtomicBroadcastClient` 
(a.k.a Orderer Client) using the `NewAtomicBroadcastClient()` function provided by the `fabric-protos-go` library 
(`orderer/ab.pb.go`).

From [hyperledger/fabric-protos-go/orderer/ab.pb.go](https://github.com/hyperledger/fabric-protos-go/blob/main/orderer/ab.pb.go)
```go
    func NewAtomicBroadcastClient(cc grpc.ClientConnInterface) AtomicBroadcastClient {
        return &atomicBroadcastClient{cc}
    }

    type AtomicBroadcastClient interface {
        // broadcast receives a reply of Acknowledgement for each common.Envelope in order, indicating success or type of failure
        Broadcast(ctx context.Context, opts ...grpc.CallOption) (AtomicBroadcast_BroadcastClient, error)
        // deliver first requires an Envelope of type DELIVER_SEEK_INFO with Payload data as a mashaled SeekInfo message, then a stream of block replies is received.
        Deliver(ctx context.Context, opts ...grpc.CallOption) (AtomicBroadcast_DeliverClient, error)
    }
```

**b. Initiating Block Delivery Stream:**

The Sidecar calls the `Deliver()` method on the `AtomicBroadcastClient` to establish a 
gRPC stream (`AtomicBroadcast_DeliverClient`) with the Ordering Service specifically 
for receiving blocks. 

From [hyperledger/fabric-protos-go/orderer/ab.pb.go](https://github.com/hyperledger/fabric-protos-go/blob/main/orderer/ab.pb.go)

```go
    type AtomicBroadcast_DeliverClient interface {
        Send(*common.Envelope) error
        Recv() (*DeliverResponse, error)
        grpc.ClientStream
    }
```

**c. Requesting Blocks:**

Within the stream, the Sidecar first calls the `Send()` method to send an Envelope containing a 
`SeekInfo` message. This message specifies the range of blocks the Sidecar wants to receive. 
For example, when the blockchain network is started for the first time, the Sidecar would 
set the start position to block `0` and the stop position to `MaxUint64`. When the sidecar 
restarts after a failure, it determines the next needed block number and sets the start block
number accordingly. It always sets the `SeekBehavior` to `BLOCK_UNTIL_READY`.

From [hyperledger/fabric-protos-go/orderer/ab.pb.go](https://github.com/hyperledger/fabric-protos-go/blob/main/orderer/ab.pb.go) (illustrative fields)
```go
    message SeekInfo {
        SeekPosition start = 1;         // The position to start the deliver from
        SeekPosition stop = 2;          // The position to stop the deliver
        SeekBehavior behavior = 3;      // The behavior when a missing block is encountered
        SeekErrorResponse error_response = 4; // How to respond to errors
        SeekContentType content_type = 5; // Defines what type of content to deliver
    }

    enum SeekBehavior {
        BLOCK_UNTIL_READY = 0; // Default behavior, block until the next block is available
        FAIL_IF_NOT_READY = 1; // Respond with an error if the block is not found
    }

    // Simplified representation of SeekPosition (can specify oldest, newest, or a specific number)
    message SeekPosition {
      oneof Type {
        SeekNewest newest = 1;
        SeekOldest oldest = 2;
        SeekSpecified specified = 3;
      }
    }
```

**d. Receiving Blocks:**

After sending the `SeekInfo`, the Sidecar continuously calls the `Recv()` method on the stream.
The Ordering Service sends back `DeliverResponse` messages, each containing a block. If the Sidecar 
requests a future block (one not yet created), the `Recv()` call will block until that block becomes 
available on the Ordering Service.

**e. Handoff for Processing:**

Each block successfully received via `Recv()` is enqueued into an internal channel (e.g., `blocksToBeCommitted`).
This channel acts as a buffer, passing the fetched blocks to the next component within the 
Sidecar responsible for relaying them to the Coordinator.

### Task 2. Relaying Blocks to the Coordinator

The following code runs *Task 2* which relays the blocks present in 
`blocksToBeCommitted` to coordinator and receive statuses.

From [service/sidecar/sidecar.go](/service/sidecar/sidecar.go#L152)
```go
  	relayService := newRelay(c.LastCommittedBlockSetInterval, metrics)
    ...
    ...
  	g.Go(func() error {
		logger.Info("Relay the blocks to committer (from s.blockToBeCommitted) and receive the transaction status.")
		return s.relay.run(gCtx, &relayRunConfig{
			coordClient:                    coordClient,
			nextExpectedBlockByCoordinator: nextBlockNum,
			configUpdater:                  s.configUpdater,
			incomingBlockToBeCommitted:     s.blockToBeCommitted,
			outgoingCommittedBlock:         s.committedBlock,
		})
	})
```

Next, we explain the code internals.

**a. Dequeueing Blocks**: 

The component monitors and reads blocks from the `blocksToBeCommitted` input queue. This queue holds 
blocks fetched from the ordering service that are pending processing and validation via the Coordinator.

**b. Block Format Conversion**: 

Each dequeued block, originally in the native Hyperledger Fabric format, is transformed into a standardized
intermediate representation. This internal `Block` format shown below is defined by the protocol buffers in
`api/protos/protocoordinatorservice`. 

```go
type Block struct {
	Number uint64             
	Txs    []*protoblocktx.Tx 
	TxsNum []uint32           
}
```
This conversion facilitates easier processing and decouples the Sidecar's internal logic from complex Fabric block structures.

**c. In-Flight Duplicate Transaction Detection**: 

Before sending the block to the Coordinator, the relay component
performs a crucial check for duplicate transaction IDs (txIDs). This specific check focuses on identifying 
duplicates among the set of transactions currently being processed or "in-flight" (i.e., transactions 
within the block being processed and potentially others recently sent to the coordinator but not yet fully committed).
 - Important Distinction: Detecting duplicate txIDs against the already committed state (transactions persisted 
   in the ledger) is explicitly handled by a separate component, referred to as the "vc service" (likely 
   Validation/Commit Service), not by this relay component during this phase.

**d. Pipelined Sending to Coordinator**: 

The converted internal block representation is then sent to the Coordinator service for validation and consensus. 
A key aspect of this process is its pipelined nature. The relay component sends blocks to the Coordinator 
continuously as they become available from the blocksToBeCommitted queue, without waiting for the Coordinator
to return the final commitment status of previously sent blocks. This approach maximizes throughput by keeping 
the Coordinator busy.

**e. Receiving and Correlating Transaction Statuses**: 

As the Coordinator processes the transactions within the blocks it receives, it sends back status updates to the
Sidecar's relay component. These statuses indicate the outcome of each transaction (e.g., committed, invalid). 
Because of the pipelined sending, these statuses may arrive out of order relative to the block sending sequence 
and might cover transactions spanning multiple blocks. The relay component is responsible for receiving these 
statuses and correctly associating them with the corresponding transactions within the blocks it is tracking.
The component maintains the state of each block sent to the Coordinator, collecting 
the status updates for all transactions within that block using `blockWithStatus`.
```go
	blockWithStatus struct {
		block         *common.Block
		txStatus      []validationCode
		txIDToTxIndex map[string]int
		pendingCount  int
	}
```

**f. Sequential Commitment Check**: 

A block is only considered fully processed and ready for commitment when two conditions are met:

 1. Status updates for all transactions within that specific block have been received from the Coordinator.
 2. This block is the next block in the expected sequential order to be committed to the ledger. (e.g., if block 
    5 was the last committed block, the component waits until all statuses for block 6 are received before considering block 6 ready).

**g. Enqueueing Committed Blocks**: 

When a block satisfies the criteria outlined in step f, the relay component first appends the collected
transaction statuses within the metadata of the original Fabric block (sourced from `blocksToBeCommitted`). 
Subsequently, this modified block is enqueued onto the `committedBlocks` output channel.

### Task 3. Persisting Committed Block in the File System

The following code runs *Task 3* which reads blocks present in the `committedBlocks`
queue and store the block in the file system.

From [service/sidecar/sidecar.go](/service/sidecar/sidecar.go#L103)

```go
	g.Go(func() error {
		return s.ledgerService.run(gCtx, &ledgerRunConfig{
			IncomingCommittedBlock: s.committedBlock,
		})
	})
```

The `committedBlocks` channel signals to the downstream "ledger service" (also referred to as the 
block delivery service) that a new block is confirmed as committed and ready for final handling. 
The ledger service then:
  - Reads the committed block from the `committedBlocks` channel.
  - Appends the block data reliably to the local append-only file system store.

We use Fabric block store package located at https://github.com/hyperledger/fabric/common/ledger/blkstorage
to manage these blocks in the file system.

## 5. Delivering Committed Block to Registered Clients

Similar to how the Sidecar connects to the Ordering Service to fetch blocks, Clients 
can also connect to the Sidecar to fetch committed blocks. This is useful because 
transactions are submitted asynchronously to the Ordering Service, and hence, the 
Client needs to rely on the committed block stream to know whether their transaction 
was committed or aborted. The txID, generated by the Client, is used for this lookup 
within the blocks.

The Client can use the `sidecarclient` package to initiate this delivery service
with the Sidecar as follows

```go

	receivedBlocksFromLedgerService := make(chan *common.Block, 10)
	deliverClient, err := sidecarclient.New(config)
  deliverClient.Deliver(ctx,
    &DeliverConfig{
      StartBlkNum: startBlkNum,
      EndBlkNum:   broadcastdeliver.MaxBlockNum,
      OutputBlock: receivedBlocksFromLedgerService,
    }, // blocking call
  )
```

The `receivedBlocksFromLedgerService` channel would hold the received committed blocks
from the Sidecar.

Next, we explain the internals of `sidecarclient`.

**a. Creating a Sidecar Client:**

The sidecarclient creates a gRPC client connection (`grpc.ClientConnInterface`) to the Sidecar
using the IP:Port on which the ledger service (the Sidecar's block delivery service)
is listening. It creates a `DeliverClient` using the `NewDeliverClient()` function 
provided by the `fabric-protos-go`` library (`peer/events.pb.go`).

From [hyperledger/fabric-protos-go/peer/events.pb.go](https://github.com/hyperledger/fabric-protos-go/blob/main/peer/events.pb.go)
  ```go
    func NewDeliverClient(cc grpc.ClientConnInterface) DeliverClient {
      return &deliverClient{cc}
    }

    func (c *deliverClient) Deliver(ctx context.Context, opts ...grpc.CallOption) (Deliver_DeliverClient, error) {
      stream, err := c.cc.NewStream(ctx, &Deliver_ServiceDesc.Streams[0], Deliver_Deliver_FullMethodName, opts...)
      if err != nil {
        return nil, err
      }
      x := &deliverDeliverClient{stream}
      return x, nil
    }
```

**b. Initiating Block Delivery Stream:**

The sidecarclient calls the `Deliver()` method on the `DeliverClient` to establish a 
gRPC stream (`Deliver_DeliverClient`) with the Sidecar for receiving blocks.

From [hyperledger/fabric-protos-go/peer/events.pb.go](https://github.com/hyperledger/fabric-protos-go/blob/main/peer/events.pb.go)
  ```go
    type Deliver_DeliverClient interface {
      Send(*common.Envelope) error
      Recv() (*DeliverResponse, error)
      grpc.ClientStream
    }
```


**c. Requesting Blocks:**

Within the stream, the sidecarclient first calls the `Send()` method to send an Envelope containing a 
`SeekInfo` message. This message specifies the range of blocks the user/client wants to receive. 
The format of `SeekInfo` is the same as the one the used for Sidecar-to-Orderer delivery stream
described in [Fetching Blocks From the Ordering Service](#fetching-blocks-from-the-ordering-service).

**d. Receiving Blocks:**

After sending the `SeekInfo`, the sidecarclient continuously calls the `Recv()` method on the stream.
The Sidecar sends back `DeliverResponse` messages, each containing a committed block. 
If the client requests a future block (one not yet committed), the `Recv()` call will 
block until that block becomes available for delivery from the Sidecar.

## 6. Failure and Recovery

The Sidecar is designed for resilience, allowing it to recover from failures that might
occur at any point during block processing. This recovery capability relies on persistently
tracking the progress of committed blocks and maintaining up-to-date configuration in the
state database.

### A. State Persistence
Resilience is achieved by ensuring the block number of the last fully committed block is 
stored durably. The Sidecar periodically reports its commit progress to the Coordinator, 
which is responsible for persisting this block number in a state database. The Coordinator 
also typically manages access to the latest channel configuration block within the state 
database.

### B. Restart and Recovery Procedure
When the Sidecar restarts after a failure, it follows these steps:

  **a. Connect to Coordinator**: The Sidecar establishes a connection with the Coordinator service.

  **b. Fetch Latest Configuration**: The Sidecar requests the most recent channel configuration 
   block (config block) from the Coordinator. This configuration is fetched from the state 
   database (via the Coordinator) to ensure the Sidecar uses the latest Ordering Service 
   endpoints, especially if the network configuration has changed.

  **c. Fetch Next Expected Block Number**: The Sidecar queries the Coordinator to determine
   the block number of the next block it should process.

  **d. Local State Reconciliation**: Using the next expected block number received from the Coordinator (`N`),
   the Sidecar checks the block number of the latest block present in its local block store (`M`).

  **e. Fetch Missing Blocks**: If the local store is behind (`M` < `N-1`), the Sidecar connects to
   the Ordering Service (using the up-to-date endpoints obtained from the fetched config block) 
   and retrieves the missing blocks sequentially, starting from block `M+1` up to `N-1`.

  **f. Retrieve Transaction Statuses**: To ensure data integrity for the recovered range, the
   Sidecar retrieves the commit/abort statuses for transactions within the newly
   fetched blocks. This status information is obtained by querying the state database via the Coordinator.

  **g. Resume Operation**: Once the local block store is synchronized (missing blocks fetched)
   and the transaction statuses for the recovered range are confirmed, the Sidecar seamlessly
   resumes its standard block processing routines from block N+1.
