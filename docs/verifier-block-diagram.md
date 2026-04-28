<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Verifier Service - Block Diagram

This document provides a detailed block diagram of the verifier service components and how data flows between them.

## High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────────────
│                           VERIFIER SERVICE
│
│                        ┌────────────────┐
│                        │  Coordinator   │
│                        │  (External)    │
│                        └────────┬───────┘
│                                 │
│                                 │ Multiple gRPC Streams
│                                 │ (Load balanced)
│                    ┌────────────┼────────────┐
│                    │            │            │
│                    ▼            ▼            ▼
│         ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│         │  Verifier    │  │  Verifier    │  │  Verifier    │
│         │  Server      │  │  Server      │  │  Server      │
│         │  Instance 1  │  │  Instance 2  │  │  Instance N  │
│         └──────┬───────┘  └──────┬───────┘  └──────┬───────┘
│                │                 │                 │
│                │ New Stream      │ New Stream      │ New Stream
│                │ Creates         │ Creates         │ Creates
│                ▼                 ▼                 ▼
│         ┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│         │ Parallel        │ │ Parallel        │ │ Parallel        │
│         │ Executor        │ │ Executor        │ │ Executor        │
│         │ (Stream 1)      │ │ (Stream 2)      │ │ (Stream N)      │
│         │                 │ │                 │ │                 │
│         │ - Verifier      │ │ - Verifier      │ │ - Verifier      │
│         │ - N Workers     │ │ - N Workers     │ │ - N Workers     │
│         │ - Batcher       │ │ - Batcher       │ │ - Batcher       │
│         └─────────────────┘ └─────────────────┘ └─────────────────┘
│
│
└─────────────────────────────────────────────────────────────────────────────
```

## Detailed Component Diagram with Data Flow

```
                    ┌───────────────────┐
                    │   Coordinator     │
                    │     Service       │
                    └─────────┬─────────┘
                              │
                              │ Bidirectional gRPC Stream
                              │
┌─────────────────────────────┼─────────────────────────────────────────────────
│ VERIFIER SERVICE            │
│                             │
│  ┌──────────────────────────▼──────────────────────────────────────────────┐
│  │  VERIFIER SERVER (verifier_server.go)                                   │
│  │                                                                         │
│  │  Verifier is STATELESS. Each stream maintains its own policy state.     │
│  │                                                                         │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │
│  │  │  StartStream(stream Verifier_StartStreamServer)                    │ │
│  │  │  - Creates new parallelExecutor for this stream                    │ │
│  │  │  - Launches goroutines using errgroup:                             │ │
│  │  │    • handleInputs()                                                │ │
│  │  │    • handleOutputs()                                               │ │
│  │  │    • handleCutoff()                                                │ │
│  │  │    • handleChannelInput() × Parallelism (default: 4 workers)       │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │
│  │                                                                         │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │
│  │  │  handleInputs()                                                    │ │
│  │  │  ┌──────────────────────────────────────────────────────────────┐  │ │
│  │  │  │  1. Receive VerifierBatch from coordinator                   │  │ │
│  │  │  │     - Contains: Update + Requests[]                          │  │ │
│  │  │  │     - FIRST batch MUST include policy updates (piggybacked)  │  │ │
│  │  │  │       to initialize the stream                               │  │ │
│  │  │  │                                                              │  │ │
│  │  │  │  2. Update Policies (if Update present)                      │  │ │
│  │  │  │     executor.verifier.updatePolicies(batch.Update)           │  │ │
│  │  │  │     - Config updates: MSP Manager, LifecycleEndorsement      │  │ │
│  │  │  │     - Namespace policies: MSP/Threshold rules                │  │ │
│  │  │  │                                                              │  │ │
│  │  │  │  3. Forward Requests to Executor                             │  │ │
│  │  │  │     for each request in batch.Requests:                      │  │ │
│  │  │  │       input.Write(request) → executor.inputCh                │  │ │
│  │  │  └──────────────────────────────────────────────────────────────┘  │ │
│  │  └────────────────┬───────────────────────────────────────────────────┘ │
│  │                   │                                                     │
│  │                   │ inputCh (buffered channel)                          │
│  │                   │ capacity = ChannelBufferSize × Parallelism          │
│  │                   │                                                     │
│  │  ┌────────────────▼───────────────────────────────────────────────────┐ │
│  │  │  PARALLEL EXECUTOR (parallel_executor.go)                          │ │
│  │  │                                                                    │ │
│  │  │  ┌───────────────────────────────────────────────────────────────┐ │ │
│  │  │  │  handleChannelInput() [Worker 1..N]                           │ │ │
│  │  │  │  ┌─────────────────────────────────────────────────────────┐  │ │ │
│  │  │  │  │  Loop:                                                  │  │ │ │
│  │  │  │  │    1. Read TxWithRef from inputCh                       │  │ │ │
│  │  │  │  │    2. Call verifier.verifyRequest(tx)                   │  │ │ │
│  │  │  │  │    3. Determine if config TX (isConfig flag)            │  │ │ │
│  │  │  │  │    4. Write verificationOutput to outputSingleCh        │  │ │ │
│  │  │  │  └─────────────────────────────────────────────────────────┘  │ │ │
│  │  │  └────────────────┬──────────────────────────────────────────────┘ │ │
│  │  │                   │                                                │ │
│  │  │                   │ outputSingleCh (buffered channel)              │ │
│  │  │                   │ capacity = ChannelBufferSize × Parallelism     │ │
│  │  │                   │                                                │ │
│  │  │  ┌────────────────▼──────────────────────────────────────────────┐ │ │
│  │  │  │  handleCutoff()                                               │ │ │
│  │  │  │  ┌─────────────────────────────────────────────────────────┐  │ │ │
│  │  │  │  │  Batching Logic:                                        │  │ │ │
│  │  │  │  │                                                         │  │ │ │
│  │  │  │  │  outputBuffer []*TxStatus                               │  │ │ │
│  │  │  │  │                                                         │  │ │ │
│  │  │  │  │  Loop:                                                  │  │ │ │
│  │  │  │  │    select:                                              │  │ │ │
│  │  │  │  │      case <-time.After(BatchTimeCutoff):                │  │ │ │
│  │  │  │  │        if len(outputBuffer) > 0:                        │  │ │ │
│  │  │  │  │          emit batch                                     │  │ │ │
│  │  │  │  │                                                         │  │ │ │
│  │  │  │  │      case output := <-outputSingleCh:                   │  │ │ │
│  │  │  │  │        append to outputBuffer                           │  │ │ │
│  │  │  │  │        if output.isConfig:                              │  │ │ │
│  │  │  │  │          emit immediately (no batching)                 │  │ │ │
│  │  │  │  │        else if len(buffer) >= BatchSizeCutoff:          │  │ │ │
│  │  │  │  │          emit batch                                     │  │ │ │
│  │  │  │  └─────────────────────────────────────────────────────────┘  │ │ │
│  │  │  └────────────────┬──────────────────────────────────────────────┘ │ │
│  │  │                   │                                                │ │
│  │  │                   │ outputCh (unbuffered channel)                  │ │
│  │  │                   │                                                │ │
│  │  └───────────────────┼────────────────────────────────────────────────┘ │
│  │                      │                                                  │
│  │  ┌───────────────────▼────────────────────────────────────────────────┐ │
│  │  │  handleOutputs()                                                   │ │
│  │  │  ┌──────────────────────────────────────────────────────────────┐  │ │
│  │  │  │  Loop:                                                       │  │ │
│  │  │  │    1. Read batch from outputCh                               │  │ │
│  │  │  │    2. Update metrics (VerifierServerOutTxs)                  │  │ │
│  │  │  │    3. Send TxStatusBatch to coordinator via stream.Send()    │  │ │
│  │  │  └──────────────────────────────────────────────────────────────┘  │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │
│  └─────────────────────────────────────────────────────────────────────────┘
│
│  ┌─────────────────────────────────────────────────────────────────────────┐
│  │  VERIFIER CORE (verify.go)                                              │
│  │                                                                         │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │
│  │  │  verifier struct                                                   │ │
│  │  │  - verifiers: atomic.Pointer[map[string]*NsVerifier]               │ │
│  │  │  - bundle: *channelconfig.Bundle (MSP Manager)                     │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │
│  │                                                                         │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │
│  │  │  updatePolicies(update *VerifierUpdates)                           │ │
│  │  │  ┌──────────────────────────────────────────────────────────────┐  │ │
│  │  │  │  1. Update Bundle (if Config present)                        │  │ │
│  │  │  │     - Parse config envelope                                  │  │ │
│  │  │  │     - Create new channelconfig.Bundle                        │  │ │
│  │  │  │     - Extract MSP Manager                                    │  │ │
│  │  │  │                                                              │  │ │
│  │  │  │  2. Create New Verifiers                                     │  │ │
│  │  │  │     if Config:                                               │  │ │
│  │  │  │       - Parse LifecycleEndorsement policy                    │  │ │
│  │  │  │       - Create verifier for MetaNamespaceID                  │  │ │
│  │  │  │     if NamespacePolicies:                                    │  │ │
│  │  │  │       - For each policy item:                                │  │ │
│  │  │  │         • Parse namespace policy                             │  │ │
│  │  │  │         • Create NsVerifier                                  │  │ │
│  │  │  │                                                              │  │ │
│  │  │  │  3. Merge with Existing Verifiers                            │  │ │
│  │  │  │     - Keep existing verifiers not in update                  │  │ │
│  │  │  │     - Update MSP Manager for signature policies              │  │ │
│  │  │  │     - Store new verifiers map atomically                     │  │ │
│  │  │  └──────────────────────────────────────────────────────────────┘  │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │
│  │                                                                         │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │
│  │  │  verifyRequest(tx *TxWithRef) → *TxStatus                          │ │
│  │  │  ┌──────────────────────────────────────────────────────────────┐  │ │
│  │  │  │  Load current verifiers map (atomic read)                    │  │ │
│  │  │  │                                                              │  │ │
│  │  │  │  For each namespace in tx.Content.Namespaces:                │  │ │
│  │  │  │    1. Skip ConfigNamespaceID (verified by orderer)           │  │ │
│  │  │  │                                                              │  │ │
│  │  │  │    2. Lookup namespace verifier                              │  │ │
│  │  │  │       if not found:                                          │  │ │
│  │  │  │         return ABORTED_SIGNATURE_INVALID                     │  │ │
│  │  │  │                                                              │  │ │
│  │  │  │    3. Verify namespace signatures                            │  │ │
│  │  │  │       nsVerifier.VerifyNs(txID, tx, nsIndex)                 │  │ │
│  │  │  │       if error:                                              │  │ │
│  │  │  │         return ABORTED_SIGNATURE_INVALID                     │  │ │
│  │  │  │                                                              │  │ │
│  │  │  │  Return COMMITTED if all verifications pass                  │  │ │
│  │  │  └──────────────────────────────────────────────────────────────┘  │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │
│  └─────────────────────────────────────────────────────────────────────────┘
│
│  ┌─────────────────────────────────────────────────────────────────────────┐
│  │  POLICY MANAGEMENT (policy/policy.go)                                   │
│  │                                                                         │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │
│  │  │  CreateNamespaceVerifier(pd, idDeserializer)                       │ │
│  │  │  - Validates namespace ID (length, characters)                     │ │
│  │  │  - Unmarshals NamespacePolicy proto                                │ │
│  │  │  - Creates NsVerifier with policy and MSP Manager                  │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │
│  │                                                                         │
│  │  ┌────────────────────────────────────────────────────────────────────┐ │
│  │  │  ParseLifecycleEndorsementPolicy(bundle)                           │ │
│  │  │  - Extracts LifecycleEndorsement policy from channel config        │ │
│  │  │  - Creates NsVerifier for MetaNamespaceID                          │ │
│  │  └────────────────────────────────────────────────────────────────────┘ │
│  └─────────────────────────────────────────────────────────────────────────┘
│
└─────────────────────────────────────────────────────────────────────────────
```

## Policy Update Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        POLICY UPDATE MECHANISM                              │
└─────────────────────────────────────────────────────────────────────────────┘

Coordinator sends VerifierBatch with Update:
  │
  ├─► Config Update (update.Config != nil)
  │   │
  │   ├─► Parse config envelope
  │   ├─► Create new channelconfig.Bundle
  │   ├─► Extract MSP Manager (identity deserializer)
  │   ├─► Parse LifecycleEndorsement policy
  │   └─► Create verifier for MetaNamespaceID
  │
  └─► Namespace Policy Update (update.NamespacePolicies != nil)
      │
      └─► For each PolicyItem:
          ├─► Validate namespace ID
          ├─► Unmarshal NamespacePolicy proto
          ├─► Determine policy type:
          │   ├─► MspRule: MSP-based signature policy
          │   └─► ThresholdRule: Threshold signature policy
          └─► Create NsVerifier for namespace

Merge with Existing Verifiers:
  │
  ├─► Keep existing verifiers not in update
  ├─► Update MSP Manager for signature policies (if Config update)
  └─► Store new verifiers map atomically

NOTE: Policy updates are applied just-in-time:
  - Sidecar waits for all preceding TXs to complete before submitting config TX
  - Verifier applies update immediately before processing next data TX
  - Ensures all subsequent TXs use the new configuration
```

## Signature Verification Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    SIGNATURE VERIFICATION PROCESS                           │
└─────────────────────────────────────────────────────────────────────────────┘

Transaction arrives with multiple namespaces:
  │
  └─► For each namespace in TX:
      │
      ├─► Skip ConfigNamespaceID
      │   └─► Config TXs verified by ordering service
      │
      ├─► Lookup namespace verifier
      │   ├─► If not found: ABORTED_SIGNATURE_INVALID
      │   └─► If found: proceed to verification
      │
      └─► Verify namespace signatures
          │
          ├─► Extract namespace data (ASN1 encoding)
          │   └─► Includes: TxID, NsID, NsVersion, Read/ReadWrites/BlindWrites
          │
          ├─► Get endorsements for this namespace
          │   └─► tx.Endorsements[nsIndex]
          │
          └─► Apply policy verification:
              │
              ├─► MSP-Based Policy (MspRule)
              │   ├─► Deserialize creator identities
              │   ├─► Verify signatures using MSP
              │   └─► Evaluate policy (AND/OR/NOutOf)
              │
              └─► Threshold Policy (ThresholdRule)
                  └─► Verify threshold signature (ECDSA/EdDSA/BLS)

Result:
  ├─► All namespaces valid: COMMITTED
  └─► Any namespace invalid: ABORTED_SIGNATURE_INVALID
```

## Batching and Cutoff Logic

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         BATCHING MECHANISM                                  │
└─────────────────────────────────────────────────────────────────────────────┘

Configuration (ExecutorConfig):
  - BatchSizeCutoff: Minimum batch size (default: 50)
  - BatchTimeCutoff: Maximum wait time (default: 500ms)
  - Parallelism: Number of worker goroutines (default: 4)
  - ChannelBufferSize: Input/output buffer size (default: 50)

Batching Strategy:

  outputBuffer []*TxStatus
  │
  ├─► Regular Transaction Received
  │   ├─► Append to outputBuffer
  │   ├─► If len(outputBuffer) >= BatchSizeCutoff:
  │   │   └─► Emit batch immediately
  │   └─► Else: wait for more transactions or timeout
  │
  ├─► Config Transaction Received
  │   ├─► Append to outputBuffer
  │   └─► Emit batch immediately (no batching for config TXs)
  │
  └─► Timeout (BatchTimeCutoff elapsed)
      └─► If len(outputBuffer) > 0:
          └─► Emit batch (even if below BatchSizeCutoff)

Rationale:
  - Regular TXs: Batching reduces gRPC overhead and improves throughput
  - Timeout: Prevents indefinite waiting when load is low
  - Config TXs: Config TXs are processed without other TXs. Nothing to batch it with.
```

## Concurrency Model

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        CONCURRENCY ARCHITECTURE                             │
└─────────────────────────────────────────────────────────────────────────────┘

Per-Stream Goroutines (using errgroup):
  │
  ├─► handleInputs() [1 goroutine]
  │   └─► Receives from gRPC stream, writes to inputCh
  │
  ├─► handleChannelInput() [N goroutines, N = Parallelism]
  │   └─► Reads from inputCh, verifies, writes to outputSingleCh
  │
  ├─► handleCutoff() [1 goroutine]
  │   └─► Reads from outputSingleCh, batches, writes to outputCh
  │
  └─► handleOutputs() [1 goroutine]
      └─► Reads from outputCh, sends to gRPC stream

Channel Flow:
  inputCh → [Workers] → outputSingleCh → [Batcher] → outputCh → gRPC

Synchronization:
  - Channels provide synchronization between goroutines
  - Atomic pointer for verifiers map (lock-free reads)
  - errgroup ensures all goroutines complete before stream closes
  - Context cancellation propagates to all goroutines

Error Handling:
  - Any goroutine error cancels the entire stream
  - ErrUpdatePolicies: Invalid policy update (INVALID_ARGUMENT)
  - Other errors: Wrapped as CANCELLED
```

## Metrics and Monitoring

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              METRICS                                        │
└─────────────────────────────────────────────────────────────────────────────┘

Throughput Metrics:
  - verifier_server_tx_in_total
    └─► Counter: Total transactions received from coordinator

  - verifier_server_tx_out_total
    └─► Counter: Total transaction statuses sent to coordinator

Active State Metrics:
  - verifier_server_grpc_active_streams
    └─► Gauge: Number of active gRPC streams

  - verifier_server_parallel_executor_active_requests
    └─► Gauge: Number of transactions currently being verified

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
  - Server.Port: gRPC server port (default: 5001)
  - Server.TLS: TLS configuration for secure connections

Parallel Executor Configuration:
  - Parallelism: Number of verification workers (default: 4)
    └─► Higher values: More concurrent verification
    └─► Lower values: Less memory usage

  - BatchSizeCutoff: Minimum batch size (default: 50)
    └─► Higher values: Better throughput, higher latency
    └─► Lower values: Lower latency, more gRPC overhead

  - BatchTimeCutoff: Maximum wait time (default: 500ms)
    └─► Higher values: Better batching, higher latency
    └─► Lower values: Lower latency, less efficient batching

  - ChannelBufferSize: Buffer size per worker (default: 50)
    └─► Total capacity = ChannelBufferSize × Parallelism
    └─► Higher values: Better handling of load spikes
    └─► Lower values: Less memory usage, more backpressure
```

## Key Design Decisions

### 1. Per-Stream Executor
Each gRPC stream gets its own `parallelExecutor` instance to ensure:
- Responses are sent to the correct stream
- No cross-stream interference
- Clean resource cleanup on stream closure

### 2. Atomic Verifiers Map
The verifiers map uses `atomic.Pointer` for lock-free reads:
- Workers can read the current verifiers without blocking
- Policy updates are atomic (all-or-nothing)
- Temporary inconsistency is acceptable (coordinator ensures ordering)

### 3. Config TX Priority
Config transactions bypass batching for immediate processing:
- Ensures policy updates are applied as soon as possible
- Prevents dependent data TXs from being delayed
- Coordinator waits for config TX completion before sending dependent TXs

### 4. Just-In-Time Policy Updates
Policy updates are applied immediately before processing the next data TX:
- Sidecar ensures all preceding TXs are complete before submitting config TX
- Verifier applies update in `handleInputs()` before forwarding requests
- Guarantees all subsequent TXs use the new configuration

### 5. Namespace Verification Independence
Each namespace in a transaction is verified independently:
- Allows different namespaces to have different policies
- Single invalid namespace invalidates the entire transaction
- Supports multi-namespace transactions with heterogeneous policies
