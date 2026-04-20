<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Sidecar Service - Block Diagram

This document provides a detailed block diagram of the sidecar service components and how data flows between them.

## High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           SIDECAR SERVICE                                   │
│                                                                             │
│  ┌────────────────┐      ┌──────────────┐       ┌─────────────────┐         │
│  │  Orderer       │      │ Coordinator  │       │   Client Apps   │         │
│  │  (External)    │      │ (External)   │       │   (External)    │         │
│  └────────┬───────┘      └──────────────┘       └────────┬────────┘         │
│           │                     ▲ ◄──── Blocks           │                  │
│           │ Blocks              │                        │ Block Queries    │
│           │                     │                        │ & Notifications  │
│           ▼                     ▼ ────► Tx Status        ▼                  │
│  ┌────────────────┐    ┌────────────────┐     ┌──────────────────┐          │
│  │  Delivery      │    │     Relay      │     │  Block Delivery  │          │
│  │  (from         │───▶│   Component    │────▶│  & Block Query   │          │
│  │   Orderer)     │    │ (Bidirectional │     │                  │          │
│  └────────────────┘    │  gRPC Stream)  │     └──────────────────┘          │
│                        └───┬────────┬───┘              ▲                    │
│                            │        │                  │                    │
│                Status      │        │ Committed        │                    │
│                Updates     │        │ Blocks           │                    │
│                            │        │                  │                    │
│                            ▼        ▼                  │                    │
│              ┌────────────────┐  ┌────────────────┐    │                    │
│              │   Notifier     │  │  Block Store   │────┘                    │
│              │   Component    │  │   (Ledger)     │                         │
│              └────────────────┘  └────────────────┘                         │
│                                                                             │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Detailed Component Diagram with Data Flow

```
                    ┌───────────────────┐                    ┌─────────────┐
                    │  Orderer Service  │                    │ Coordinator │
                    │  (Fabric Network) │                    │   Service   │
                    └─────────┬─────────┘                    └──────┬──────┘
                              │                                     │
                              │ Raw Blocks                          │
                              │                                     │
                              │                                     │
┌─────────────────            │                                     │
│ SIDECAR SERVICE             │                                     │
│                             │                                     │
│  ┌──────────────────────────▼──────────────┐                      │
│  │  startDelivery()                        │                      │
│  │  - Fetches blocks from orderer          │                      │
│  │  - Verifies block signatures            │                      │
│  │  - Handles config updates               │                      │
│  └──────────────────┬──────────────────────┘                      │
│                     │                                             │
│                     │ Blocks                                      │
│                     │ (chan *common.Block)                        │
│                     │                                             │
│  ┌──────────────────▼──────────────────────────────────────────┐  │
│  │  RELAY COMPONENT                                            │  │
│  │  ┌────────────────────────────────────────────────────────┐ │  │
│  │  │  preProcessBlock()                                     │ │  │
│  │  │  - Maps blocks using mapBlock()                        │ │  │
│  │  │  - Validates transaction form                          │ │  │
│  │  │  - Detects duplicates (txIDToHeight map)               │ │  │
│  │  │  - Handles config blocks separately                    │ │  │
│  │  │  - Manages waitingTxsSlots (backpressure)              │ │  │
│  │  └────────────────┬───────────────────────────────────────┘ │  │
│  │                   │                                         │  │
│  │                   │ mappedBlockQueue                        │  │
│  │                   │ (chan *blockMappingResult)              │  │
│  │                   │                                         │  │
│  │  ┌────────────────▼───────────────────────────────────────┐ │  │
│  │  │  sendBlocksToCoordinator()                             │ │  │
│  │  │  - Stores blocks in blkNumToBlkWithStatus map          │ │  │
│  │  │  - Sends CoordinatorBatch via gRPC stream ─────────────┼─┼─►│
│  │  │  - Tracks pending transactions                         │ │  │
│  │  └────────────────────────────────────────────────────────┘ │  │
│  │                                                             │  │
│  │  ┌────────────────────────────────────────────────────────┐ │  │
│  │  │  receiveStatusFromCoordinator()                        │ │  │
│  │  │  - Receives TxStatusBatch from coordinator ◄───────────┼─┼──│
│  │  │  - Writes to statusBatch channel                       │ │  │
│  │  └────────────────┬───────────────────────────────────────┘ │  │
│  │                   │                                         │  │
│  │                   │ statusBatch                             │  │
│  │                   │ (chan *committerpb.TxStatusBatch)       │  │
│  │                   │                                         │  │
│  │  ┌────────────────▼───────────────────────────────────────┐ │  │
│  │  │  processStatusBatch()                                  │ │  │
│  │  │  - Updates blockWithStatus.txStatus                    │ │  │
│  │  │  - Removes from txIDToHeight map                       │ │  │
│  │  │  - Releases waitingTxsSlots                            │ │  │
│  │  │  - Calls processCommittedBlocksInOrder()               │ │  │
│  │  └────────────────┬───────────────┬───────────────────────┘ │  │
│  │                   │               │                         │  │
│  │                   │               │ statusQueue             │  │
│  │                   │               │ (chan []*TxStatus)      │  │
│  │                   │               │                         │  │
│  │                   │               │                         │  │
│  │  ┌────────────────▼───────────────────────────────────────┐ │  │
│  │  │  setLastCommittedBlockNumber()                         │ │  │
│  │  │  - Periodically updates coordinator ───────────────────┼─┼─►│
│  │  │  - Batches block number updates                        │ │
│  │  └────────────────────────────────────────────────────────┘ │
│  └─────────────────────────────────────────────────────────────┘
│
│                     BIDIRECTIONAL gRPC STREAM
│                     ═══════════════════════════
│                     Relay ──► Coordinator: CoordinatorBatch
│                     Relay ◄── Coordinator: TxStatusBatch
│                     Relay ──► Coordinator: SetLastCommittedBlockNumber
│
│                 │                                │
│                 │ committedBlock                 │ statusQueue
│                 │ (chan *common.Block)           │ (chan []*committerpb.TxStatus)
│                 │                                │
│  ┌──────────────▼───────────────────┐  ┌─────────▼────────────────┐
│  │  BLOCK STORE                     │  │  NOTIFIER                │
│  │  ┌────────────────────────────┐  │  │  ┌──────────────────────┐│
│  │  │  run()                     │  │  │  │  run()               ││
│  │  │  - Receives committed      │  │  │  │  - Manages           ││
│  │  │    blocks                  │  │  │  │    subscriptions     ││
│  │  │  - Appends to FileLedger   │  │  │  │  - Matches txIDs     ││
│  │  │  - Manages fsync interval  │  │  │  │  - Handles timeouts  ││
│  │  │  - Updates block height    │  │  │  │  - Sends responses   ││
│  │  └────────────────────────────┘  │  │  └──────────────────────┘│
│  │                                  │  │                          │
│  │  ┌────────────────────────────┐  │  │  ┌──────────────────────┐│
│  │  │  FileLedger                │  │  │  │  subscriptions       ││
│  │  │  - Block storage           │  │  │  │  - txID → requests   ││
│  │  │  - Block indexing          │  │  │  │  - availableSlots    ││
│  │  │  - Height tracking         │  │  │  └──────────────────────┘│
│  │  └────────────────────────────┘  │  │                          │
│  └──────────────┬───────────────────┘  └──────────┬───────────────┘
│                 │                                 │
│                 │                                 │
│  ┌──────────────▼───────────────┐  ┌──────────────▼───────────────┐
│  │  BLOCK DELIVERY              │  │  NOTIFICATION STREAMS        │
│  │  - Implements peer.Deliver   │  │  - OpenNotificationStream()  │
│  │  - Streams blocks to clients │  │  - Bidirectional gRPC        │
│  │  - Supports seek operations  │  │  - Per-client queues         │
│  └──────────────┬───────────────┘  └──────────────┬───────────────┘
│                 │                                 │
│  ┌──────────────▼───────────────┐                 │
│  │  BLOCK QUERY                 │                 │
│  │  - GetBlockchainInfo()       │                 │
│  │  - GetBlockByNumber()        │                 │
│  │  - GetBlockByTxID()          │                 │
│  │  - GetTxByID()               │                 │
│  └──────────────┬───────────────┘                 │
│                 │                                 │
└─────────────────┼─────────────────────────────────┼─────────────────────────────
                  │                                 │
                  │ gRPC Responses                  │ gRPC Responses
                  │                                 │
            ┌─────▼─────────────────────────────────▼─────┐
            │           CLIENT APPLICATIONS               │
            │  - Block delivery clients                   │
            │  - Query clients                            │
            │  - Notification subscribers                 │
            └─────────────────────────────────────────────┘
```

## BFT Block Puller and Block Withholding Detection

The sidecar uses a Byzantine Fault Tolerant (BFT) block delivery mechanism implemented in `utils/deliverorderer` to protect against malicious orderers that may withhold blocks.

### Architecture Overview

```
┌────────────────────────────────────────────────────────────────────────────┐
│                    BFT BLOCK DELIVERY ARCHITECTURE                         │
│                                                                            │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐    │
│  │  Orderer 1   │  │  Orderer 2   │  │  Orderer 3   │  │  Orderer N   │    │
│  │  (Party 0)   │  │  (Party 1)   │  │  (Party 2)   │  │  (Party N-1) │    │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘    │
│         │                 │                 │                 │            │
│         │ Full Blocks     │ Headers Only    │ Headers Only    │ Headers    │
│         │ (Data Stream)   │                 │                 │ Only       │
│         │                 │                 │                 │            │
│         ▼                 ▼                 ▼                 ▼            │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │              Joint Output Block Queue                                │  │
│  │              (chan *BlockWithSourceID)                               │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                    │                                       │
│                                    │                                       │
│                                    ▼                                       │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │                    Block Processing Logic                            │  │
│  │                                                                      │  │
│  │  ┌────────────────────────────────────────────────────────────────┐  │  │
│  │  │  Data Stream State Machine                                     │  │  │
│  │  │  - Verifies full blocks from current data source               │  │  │
│  │  │  - Tracks nextBlockNum, configState                            │  │  │
│  │  │  - Updates on successful verification                          │  │  │
│  │  └────────────────────────────────────────────────────────────────┘  │  │
│  │                                                                      │  │
│  │  ┌────────────────────────────────────────────────────────────────┐  │  │
│  │  │  Header-Only Stream State Machine                              │  │  │
│  │  │  - Verifies block headers from all other orderers              │  │  │
│  │  │  - Tracks nextBlockNum, configState                            │  │  │
│  │  │  - Aggregates progress from multiple sources                   │  │  │
│  │  └────────────────────────────────────────────────────────────────┘  │  │
│  │                                                                      │  │
│  │  ┌────────────────────────────────────────────────────────────────┐  │  │
│  │  │  Block Withholding Detection                                   │  │  │
│  │  │  - Monitors: targetNextBlockNum, targetArrivalTime             │  │  │
│  │  │  - Compares data stream vs header-only stream progress         │  │  │
│  │  │  - Triggers source switch on suspicion                         │  │  │
│  │  └────────────────────────────────────────────────────────────────┘  │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                    │                                       │
│                                    │ Verified Blocks                       │
│                                    ▼                                       │
│                          ┌──────────────────┐                              │
│                          │  Output Channel  │                              │
│                          │  (to Sidecar)    │                              │
│                          └──────────────────┘                              │
└────────────────────────────────────────────────────────────────────────────┘
```

### Block Withholding Detection Mechanism

The BFT block puller uses parallel streams to detect when an orderer is withholding blocks:

```
Timeline View:

Orderer 1 (Data Source):     Block 100 ──────────────────────────────► Block 101 (delayed)
                                 │                                          │
                                 │                                          │
Orderer 2 (Header-Only):      Block 100 ──► Block 101 ──► Block 102 ──► Block 103
                                 │              │                           │
                                 │              │                           │
Orderer 3 (Header-Only):      Block 100 ──► Block 101 ──► Block 102 ──► Block 103
                                 │              │                           │
                                 │              │                           │
Detection Logic:                 │              │                           │
                                 │              │                           │
                                 │         Set Target:                 Suspicion!
                                 │         Block 101                   Switch to
                                 │         Deadline: T+1Δ              Orderer 2
                                 │                                          │
                                 └──────────────────────────────────────────┘
                                        Grace Period
                                    (1 × SuspicionGracePeriodPerBlock)
```

### Detection Algorithm

1. **Target Setting**: When header-only streams are ahead of the data stream:
   ```
   targetNextBlockNum = headerOnlyStream.nextBlockNum
   gap = targetNextBlockNum - dataStream.nextBlockNum
   targetArrivalTime = now + (SuspicionGracePeriodPerBlock × gap)
   ```

2. **Monitoring**: On each block or timeout:
   - If `dataStream.nextBlockNum >= targetNextBlockNum`: Reset target (data caught up)
   - If `now < targetArrivalTime`: Continue waiting (still within grace period)
   - If `now >= targetArrivalTime`: Raise suspicion and switch data source

3. **Source Switching**: When suspicion is raised:
   - Log the suspected orderer and the block number
   - Clear the suspicion state (`targetNextBlockNum = 0`)
   - Restart streams with a different orderer as the data source
   - The new orderer gets a fresh grace period to deliver blocks

### Key Components

#### Fault Tolerance Levels

| Level   | Verification | Withholding Detection | Use Case                       |
| ------- | ------------ | --------------------- | ------------------------------ |
| **BFT** | ✓            | ✓                     | Production (Byzantine faults)  |
| **CFT** | ✓            | ✗                     | Production (Crash faults only) |
| **NO**  | ✗            | ✗                     | Testing only                   |

### Configuration Parameters

- **FaultToleranceLevel**: `"BFT"`, `"CFT"`, or `"NO"` (default: `"BFT"`)
- **SuspicionGracePeriodPerBlock**: Time allowed per block gap (default: 1 second)
  - Should account for: network latency + block delivery time + processing overhead
  - Too short: unnecessary source switching
  - Too long: delayed detection of actual withholding

### Stream Lifecycle

```
┌─────────────────────────────────────────────────────────────────────┐
│                      Stream Lifecycle                               │
└─────────────────────────────────────────────────────────────────────┘

1. Initialization (initStreams)
   ├─► Pick data block source (random or most advanced)
   ├─► Create data stream worker (full blocks)
   └─► Create header-only stream workers (N-1 orderers)

2. Concurrent Execution
   ├─► startSingleDeliveryStream() - Data stream goroutine
   ├─► startHeadersStreams() - Header-only streams goroutines
   └─► processBlocks() - Block processing goroutine

3. Block Processing (processNextBlock)
   ├─► Read from jointOutputBlock queue
   ├─► Route to appropriate state machine (data vs header)
   ├─► Verify block/header
   ├─► Update state
   ├─► Check for config updates
   └─► Check for block withholding

4. Restart Triggers
   ├─► Config Update (errConfigUpdate)
   │   └─► New endpoints/credentials detected
   ├─► Block Withholding Suspicion (errSuspicion)
   │   └─► Data source suspected of withholding
   └─► Data Block Error (errDataBlockError)
       └─► Verification failure on data stream

5. Recovery
   └─► restarts from step 1
```

### Error Handling

- **Data Stream Errors**: Trigger immediate restart (critical for progress)
- **Header-Only Stream Errors**: Ignored individually (prevents DOS attacks)
- **All Header Streams Fail**: Triggers restart
- **Config Updates**: Graceful restart with new configuration
- **Suspicion**: Restart with different data source

### Benefits

1. **Byzantine Fault Tolerance**: Detects and mitigates block withholding attacks
2. **Automatic Recovery**: Switches to honest orderers without manual intervention
3. **Minimal Overhead**: Header-only streams use significantly less bandwidth
4. **Configurable Sensitivity**: Adjustable grace period based on network conditions
5. **DOS Protection**: Individual header stream failures don't trigger restarts

## Resiliency and Recovery

The sidecar implements comprehensive recovery mechanisms to handle various failure scenarios and ensure data consistency across restarts and connection failures.

### Recovery Scenarios

#### 1. Sidecar Restart (Server Crash)

When the sidecar restarts after a crash, it follows a systematic recovery process:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    SIDECAR RESTART RECOVERY FLOW                            │
└─────────────────────────────────────────────────────────────────────────────┘

1. Load Latest Config Block
   ├─► Read from block store (getPrevBlockAndItsConfig)
   ├─► Extract orderer endpoints
   ├─► Extract verification credentials
   └─► Initialize TLS configuration

2. Wait for Coordinator to Become Idle
   ├─► Query: NumberOfWaitingTransactionsForStatus()
   ├─► Wait until count == 0
   └─► Ensures all in-flight transactions are processed

3. Query Coordinator State
   ├─► Call: GetNextBlockNumberToCommit()
   └─► Returns: stateDBHeight (world state height)

4. Compare Heights
   ├─► blockStoreHeight = blockStore.GetBlockHeight()
   └─► stateDBHeight = coordinator's next expected block

5. Recovery Path Selection
   │
   ├─► Case A: blockStoreHeight < stateDBHeight
   │   │       (Block store is behind)
   │   │
   │   ├─► Fetch Missing Blocks from Orderer
   │   │   └─► startDelivery(blockStoreHeight)
   │   │
   │   ├─► For Each Missing Block:
   │   │   ├─► Map block (validate transactions)
   │   │   ├─► Query status from coordinator
   │   │   │   └─► GetTransactionsStatus(txIDs)
   │   │   ├─► Fill block metadata with statuses
   │   │   └─► Append to block store
   │   │
   │   └─► Resume Normal Operation
   │
   ├─► Case B: blockStoreHeight >= stateDBHeight
   │   │       (Block store is ahead or equal)
   │   │
   │   ├─► Block store is up-to-date or ahead
   │   │   (Ahead occurs due to periodic state DB updates)
   │   │
   │   └─► Resume Normal Operation
   │       └─► Relay will submit blocks starting from stateDBHeight
   │
   └─► Case C: Config Block Update During Recovery
       │
       ├─► Extract orderer endpoints from latest config
       ├─► Update delivery parameters
       └─► Ensures connectivity with current orderers
```

**Key Recovery Steps:**

1. **Load Latest Config Block**
   ```go
   lastBlock, nextBlockVerificationConfig, err := s.getPrevBlockAndItsConfig(nextBlockNum)
   ```
   - Reads the most recent config block from the block store
   - Extracts orderer endpoints and verification credentials
   - Ensures connectivity even if endpoints changed during downtime

2. **Wait for Idle Coordinator**
   ```go
   waitForIdleCoordinator(ctx, coordClient)
   ```
   - Prevents recovery race conditions
   - Ensures all previously submitted transactions are fully processed
   - Guarantees complete status information is available

3. **Determine Recovery Point**
   ```go
   blkInfo, err := coordClient.GetNextBlockNumberToCommit(ctx, nil)
   blockStoreHeight := s.blockStore.GetBlockHeight()
   ```
   - Coordinator's `nextBlockNum` represents the world state height
   - Block store height represents local ledger state
   - Gap indicates missing blocks that need recovery

4. **Recover Missing Blocks** (if block store is behind)
   ```go
   for each missing block:
       - Fetch block from orderer (startDelivery)
       - Map and validate transactions (mapBlock)
       - Query transaction statuses from coordinator
       - Fill block metadata with statuses
       - Append to block store
   ```

#### 2. Coordinator Connection Failure

When the connection to the coordinator fails, the sidecar treats it as a potential coordinator restart:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│              COORDINATOR CONNECTION FAILURE RECOVERY                        │
└─────────────────────────────────────────────────────────────────────────────┘

Connection Failure Detected
   │
   ├─► Relay goroutines return error
   │   └─► Stream context cancelled
   │
   ├─► retry.Sustain() catches error
   │   └─► Initiates reconnection with backoff
   │
   └─► Run Full Recovery Procedure
       │
       ├─► Same as sidecar restart recovery
       │   ├─► Wait for coordinator to become idle
       │   ├─► Query next expected block number
       │   ├─► Compare with block store height
       │   └─► Recover missing blocks if needed
       │
       └─► Rationale: Coordinator may have lost data
           └─► Ensures consistency regardless of failure cause
```

**Why Full Recovery?**
- The coordinator might have restarted and lost in-memory state
- The coordinator's state database might be behind the sidecar's block store
- Full recovery ensures both systems are synchronized

#### 3. Orderer Connection Failure

Orderer connection failures are handled by the BFT block puller with automatic reconnection:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                ORDERER CONNECTION FAILURE RECOVERY                          │
└─────────────────────────────────────────────────────────────────────────────┘

Connection Failure Detected
   │
   ├─► BFT Block Puller (deliverorderer) handles reconnection
   │   │
   │   ├─► retry.Sustain() with exponential backoff
   │   │   ├─► InitialInterval: 100ms
   │   │   ├─► Multiplier: 1.5
   │   │   └─► MaxInterval: 3s
   │   │
   │   └─► Maintains Session State
   │       ├─► LastBlock: Most recently processed block
   │       ├─► NextBlockVerificationConfig: Config for verification
   │       └─► LatestKnownConfig: Newest config (for endpoints)
   │
   ├─► Reconnect to Orderer
   │   ├─► Use LatestKnownConfig for current endpoints
   │   ├─► Resume from LastBlock.Number + 1
   │   └─► Continue block verification
   │
   └─► BFT Mode: Automatic Source Switching
       ├─► If data source fails, switch to another orderer
       ├─► Header-only streams continue from other orderers
       └─► No data loss or duplicate processing
```

**Key Features:**
- **Stateful Reconnection**: Resumes from the exact point of failure
- **Config-Aware**: Uses latest config for current orderer endpoints
- **BFT Protection**: Automatically switches to healthy orderers
- **No Duplicate Processing**: Tracks last processed block number

### Recovery Guarantees

#### Block Store Consistency

```
Invariant: blockStore.Height() <= coordinator.NextBlockNumberToCommit()

Scenarios:
1. Normal Operation:
   blockStore.Height() ≈ coordinator.NextBlockNumberToCommit()
   (May differ by a few blocks due to periodic state DB updates)

2. After Sidecar Crash:
   blockStore.Height() < coordinator.NextBlockNumberToCommit()
   → Recovery fetches missing blocks

3. After Coordinator Crash:
   blockStore.Height() >= coordinator.NextBlockNumberToCommit()
   → Relay resubmits blocks starting from coordinator's next expected block
```

#### Transaction Status Consistency

```
For each block in block store:
- All transaction statuses are present in block metadata
- Statuses match the coordinator's world state
- No partial or missing status information

Recovery Process:
1. Fetch block from orderer (raw block without statuses)
2. Map block and extract transaction IDs
3. Query coordinator for transaction statuses
4. Fill block metadata with retrieved statuses
5. Append complete block to block store
```

### Recovery Metrics and Monitoring

The sidecar tracks recovery-related metrics:

```
Metrics:
- sidecar_coordinator_connection_status
  └─► Labels: {status="connected|disconnected"}

- sidecar_coordinator_connection_failures_total
  └─► Increments on each connection failure

- sidecar_ledger_block_height
  └─► Current block store height

- sidecar_relay_waiting_transactions_queue_size
  └─► Number of transactions awaiting status
```

### Error Handling During Recovery

```
Error Categories:

1. Retryable Errors (with backoff):
   ├─► Network connectivity issues
   ├─► Temporary coordinator unavailability
   └─► Orderer connection failures

2. Non-Retryable Errors (fail fast):
   ├─► Invalid block verification
   ├─► Corrupted block store data
   └─► Configuration errors

3. Recovery-Specific Errors:
   ├─► Missing transaction status from coordinator
   │   └─► Indicates coordinator data loss
   ├─► Block store corruption
   │   └─► Requires manual intervention
   └─► Config block parsing failure
       └─► Cannot extract orderer endpoints
```

### Best Practices for Recovery

1. **Periodic State DB Updates**: The coordinator updates its last committed block number every 5 seconds (configurable), balancing recovery speed with write overhead

2. **Block Store Ahead Tolerance**: The system tolerates the block store being ahead of the state DB by a few blocks, as this is expected during normal operation

3. **Idempotent Operations**: All recovery operations are idempotent—running recovery multiple times produces the same result

4. **Config Block Priority**: Config blocks are always synced immediately (no batching) to ensure endpoint updates are never missed

5. **Graceful Degradation**: If header-only streams fail in BFT mode, the system continues with the data stream only, degrading to CFT-level protection
```
