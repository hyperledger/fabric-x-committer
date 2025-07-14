<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Verification Service

1. [Overview](#1-overview)
    - [Core Responsibilities](#core-responsibilities)
2. [Architecture](#2-architecture)
    - [Components](#components)
    - [Workflow](#workflow)
3. [Signature Verification](#3-signature-verification)
    - [Policy Management](#policy-management)
    - [Verification Process](#verification-process)
4. [Parallel Execution](#4-parallel-execution)
    - [Execution Model](#execution-model)
    - [Batching Strategy](#batching-strategy)
5. [Configuration](#5-configuration)
6. [Monitoring and Metrics](#6-monitoring-and-metrics)
7. [Error Handling and Recovery](#7-error-handling-and-recovery)
8. [Implementation Details](#8-implementation-details)
    - [Core Components](#core-components)
    - [Transaction Flow](#transaction-flow)
    - [Policy Management](#policy-management)
    - [Verification Logic](#verification-logic)

## 1. Overview

The Verification Service is a component in the Fabric-X Committer architecture responsible for validating transaction 
signatures against namespace policies.
It ensures that only properly authorized transactions are committed to the state database by verifying
signatures against the appropriate policies.

### Core Responsibilities

The Verification Service performs several key functions:

1. **Policy Management**: Maintains and updates signature verification policies for different namespaces.
2. **Signature Verification**: Validates transaction signatures against the appropriate namespace policies.
3. **Parallel Processing**: Efficiently processes verification requests using parallel execution.
4. **Batched Responses**: Optimizes performance by batching verification responses.

## 2. Architecture

### Components

The Verification Service consists of several key components:

1. **Server**: The main entry point that implements the gRPC `VerifierServer` interface.
It handles incoming verification requests and manages the verification streams.
2. **Verifier**: The core component responsible for signature verification.
It maintains a map of namespace policies and performs the actual signature validation.
3. **Parallel Executor**: Manages parallel processing of verification requests to optimize throughput.
It handles input distribution, verification execution, and output collection.
4. **Policy Manager**: Handles policy updates and parsing from configuration transactions and namespace policies.

### Workflow

The Verification Service operates with the following workflow:

1. **Stream Initialization**: Clients establish a bidirectional gRPC stream with the service.
2. **Request Processing**: The service receives batches of verification requests through the stream.
3. **Policy Updates**: If included in the request batch, policy updates are processed and applied.
4. **Parallel Verification**: Verification requests are distributed to worker goroutines for parallel processing.
5. **Response Batching**: Verification results are collected, batched, and sent back to the client.

## 3. Signature Verification

### Policy Management

The Verification Service maintains policies for different namespaces in an atomic map structure.
Policies can be updated through two mechanisms:

1. **Configuration Transactions**: System-wide policies are extracted from configuration transactions in the `config` namespace.
2. **Namespace Policies**: Individual namespace policies are managed through the `meta` namespace.

The service uses a lock-free design with an atomic pointer to update the policy map without blocking ongoing verifications.

### Verification Process

The verification process follows these steps:

1. **Validate Format**: Check that the transaction is well-formed.
2. **Policy Lookup**: For each namespace in a transaction, the service looks up the corresponding policy.
3. **Signature Validation**: The service validates the transaction signatures against the namespace policy.
4. **Result Determination**: If it is well-formed, and all signatures are valid, 
the transaction is marked as `COMMITTED`. Otherwise, it's marked as `ABORTED_<reason>`. 

## 4. Parallel Execution

### Execution Model

The Verification Service uses a parallel execution model to maximize throughput:

1. **Worker Pool**: The service maintains a pool of worker goroutines, with the size determined by the `parallelism` configuration.
2. **Channel-Based Communication**: Workers communicate through channels to distribute work and collect results.

The parallel execution flow is as follows:

1. **Input Distribution**: Incoming requests are distributed to worker goroutines through an input channel.
2. **Parallel Processing**: Each worker independently verifies its assigned transactions.
3. **Result Collection**: Verification results are collected through an output channel.
4. **Response Batching**: Results are batched before being sent back to the client.

### Batching Strategy

The service employs a batching strategy to optimize performance:

1. **Size-Based Batching**: Responses are batched until they reach a configurable size threshold (`BatchSizeCutoff`).
2. **Time-Based Batching**: If the size threshold isn't reached within a configurable time window (`BatchTimeCutoff`), the batch is sent anyway.
3. **Buffer Management**: The service maintains buffers for both input and output to handle load fluctuations.

## 5. Configuration

The Verification Service is configured through a YAML file with the following key parameters:

```yaml
server:
  address: ":5001"  # Network address for the gRPC server

monitoring:
  server:
    address: ":2115"  # Address for Prometheus metrics

parallel-executor:
  parallelism: 8  # Number of worker goroutines
  batch-size-cutoff: 100  # Minimum batch size for responses
  batch-time-cutoff: 10ms  # Maximum wait time for batching
  channel-buffer-size: 1000  # Size of channel buffers
```

### Configuration Parameters

- **Server**: Network configuration for the gRPC server, including address and port.
- **Monitoring**: Configuration for the Prometheus metrics server.
- **Parallel Executor**:
  - `parallelism`: Number of worker goroutines for parallel processing.
  - `batch-size-cutoff`: Minimum number of responses to trigger a batch.
  - `batch-time-cutoff`: Maximum time to wait before sending a batch.
  - `channel-buffer-size`: Size of the channel buffers for handling load fluctuations.

## 6. Monitoring and Metrics

The Verification Service exposes several Prometheus metrics to monitor its performance:

1. **Transaction Throughput**:
   - `verifier_server_tx_in`: Counter for incoming transactions.
   - `verifier_server_tx_out`: Counter for outgoing verification results.

2. **Active Streams and Requests**:
   - `verifier_server_grpc_active_streams`: Gauge for the number of active gRPC streams.
   - `verifier_server_parallel_executor_active_requests`: Gauge for the number of active verification requests.

These metrics provide insights into the service's performance and can be used for capacity planning and troubleshooting.

## 7. Error Handling and Recovery

The Verification Service implements robust error handling to ensure reliable operation:

1. **Stream Error Handling**: The service gracefully handles stream errors by cleaning up resources.
2. **Policy Update Errors**: If a policy update fails, the service returns an error message and aborts the stream.
3. **Context Cancellation**: The service properly handles context cancellation to ensure clean shutdown.
4. **Graceful Shutdown**: When the service is shutting down, it allows in-flight operations to complete or time out.

## 8. Implementation Details

### Core Components

The Verification Service is implemented in the `service/verifier` package with the following key components.

#### Server

The `Server` struct (under `verifier_server.go`) implements the gRPC server interface and stream handling.

From [fabric-x-committer/service/verifier/verifier_server.go](/service/verifier/verifier_server.go).
```go
type Server struct {
    protosigverifierservice.UnimplementedVerifierServer
    ...
}
```

It provides the following methods:
- `New(config *Config) *Server`: Creates a new server instance.
- `Run(ctx context.Context) error`: Starts the server and monitoring.
- `WaitForReady(context.Context) bool`: Checks if the server is ready.
- `StartStream(stream protosigverifierservice.Verifier_StartStreamServer) error`: Handles verification streams.

#### Verifier

The `verifier` struct (under `verify.go`) contains the core verification logic.

From [fabric-x-committer/service/verifier/verify.go](/service/verifier/verify.go).
```go
type verifier struct {
    verifiers atomic.Pointer[map[string]*signature.NsVerifier]
}
```

It provides the following methods:
- `newVerifier() *verifier`: Creates a new verifier instance.
- `updatePolicies(update *protosigverifierservice.Update) error`: Updates verification policies.
- `verifyRequest(request *protosigverifierservice.Request) *protosigverifierservice.Response`: Verifies a single request.

#### Parallel Executor

The `parallelExecutor` struct (under `parallel_executor.go`) manages parallel processing of verification requests.

From [fabric-x-committer/service/verifier/parallel_executor.go](/service/verifier/parallel_executor.go).
```go
type parallelExecutor struct {
    inputCh        chan *protosigverifierservice.Request
    outputSingleCh chan *protosigverifierservice.Response
    outputCh       chan []*protosigverifierservice.Response
    verifier       *verifier
    config         *ExecutorConfig
}
```

It provides the following methods:
- `newParallelExecutor(config *ExecutorConfig) *parallelExecutor`: Creates a new executor instance.
- `handleChannelInput(ctx context.Context)`: Processes input requests. Runs in parallel from multiple goroutines.
- `handleCutoff(ctx context.Context)`: Manages response batching.

#### Policy Management

The policy package (under `policy/...`) provides functions for policy management:
- `ParseNamespacePolicyItem(pd *protoblocktx.PolicyItem) (*signature.NsVerifier, error)`: Parses namespace policies.
- `ParsePolicyFromConfigTx(value []byte) (*signature.NsVerifier, error)`: Parses policies from config transactions.
- `GetUpdatesFromNamespace(nsTx *protoblocktx.TxNamespace) *protosigverifierservice.Update`: Extracts policy updates from namespace transactions.
- `ValidateNamespaceID(nsID string) error`: Validates namespace IDs.

### Transaction Flow

Transactions are received in batches over a bidirectional gRPC stream.
The verifier processes each transaction and sends responses back through the same stream.
The order of transactions in the response is not guaranteed to match the order of the incoming requests.

Each received transaction is added to an internal queue.
Multiple worker routines consume from this queue to perform validation and place the results into an output queue.
A separate batching worker collects validated responses from the output queue and groups them into batches.
These batches are then placed in a response queue.
Finally, a dedicated sender worker reads from the response queue and transmits the response batches over the gRPC stream.

Following is a detailed description of this flow.

#### Stream Initialization

The `StartStream` method initializes a bidirectional gRPC stream for verification.
It starts the following workers, and stops if one of them fails.

#### Request Handling

The `handleInputs` method processes incoming verification requests.
Policy updates are piggybacked over received batches, and applied before inserting the transactions to the queue.
Full description of the policy management will follow.

From [fabric-x-committer/service/verifier/verifier_server.go](/service/verifier/verifier_server.go).
```go
func (s *Server) handleInputs(
    ctx context.Context,
    stream protosigverifierservice.Verifier_StartStreamServer,
    executor *parallelExecutor,
) error {
    input := channel.NewWriter(ctx, executor.inputCh)
    for {
        batch, rpcErr := stream.Recv()
        if rpcErr != nil { ... }
        
        // Update policies if included in the batch.
        err := executor.verifier.updatePolicies(batch.Update)
        if err != nil { ... }
        
        // Pass verification requests for processing.
        for _, r := range batch.Requests {
            if ok := input.Write(r); !ok { ... }
        }
    }
}
```

#### Response Handling

The `handleOutputs` method processes verification responses:

From [fabric-x-committer/service/verifier/verifier_server.go](/service/verifier/verifier_server.go).
```go
func (s *Server) handleOutputs(
    ctx context.Context,
    stream protosigverifierservice.Verifier_StartStreamServer,
    executor *parallelExecutor,
) error {
    output := channel.NewReader(ctx, executor.outputCh)
    for {
        outputs, ok := output.Read()
        if !ok {
            return errors.Wrap(stream.Context().Err(), "context ended")
        }
        
        // Update metrics and send responses
        promutil.AddToCounter(s.metrics.VerifierServerOutTxs, len(outputs))
        promutil.AddToGauge(s.metrics.ActiveRequests, -len(outputs))
        rpcErr := stream.Send(&protosigverifierservice.ResponseBatch{Responses: outputs})
        if rpcErr != nil {
            return errors.Wrap(rpcErr, "stream ended")
        }
    }
}
```


#### Batching

The `handleCutoff` method implements response batching:

From [fabric-x-committer/service/verifier/parallel_executor.go](/service/verifier/parallel_executor.go).
```go
func (e *parallelExecutor) handleCutoff(ctx context.Context) {
    var outputBuffer []*protosigverifierservice.Response
    chOut := channel.NewWriter(ctx, e.outputCh)
    
    cutBatch := func(size int) {
        for len(outputBuffer) >= size {
            batchSize := min(e.config.BatchSizeCutoff, len(outputBuffer))
            chOut.Write(outputBuffer[:batchSize])
            outputBuffer = outputBuffer[batchSize:]
        }
    }
    for {
        select {
        case <-ctx.Done():
            return
        case <-time.After(e.config.BatchTimeCutoff):
            // Time-based batching
            cutBatch(1)
        case output := <-e.outputSingleCh:
            // Size-based batching
            outputBuffer = append(outputBuffer, output)
            cutBatch(e.config.BatchSizeCutoff)
        }
    }
}
```

This implementation balances throughput and latency by using both size-based and time-based batching.

#### Worker Pool

The service uses a fixed-size worker pool for parallel processing:

From [fabric-x-committer/service/verifier/parallel_executor.go](/service/verifier/parallel_executor.go).
```go
for range executor.config.Parallelism {
    g.Go(func() error {
        executor.handleChannelInput(gCtx)
        return gCtx.Err()
    })
}
```

Each worker processes requests independently.

### Policy Management

#### Policy Storage

Policies are stored in an atomic map to allow lock-free updates:

From [fabric-x-committer/service/verifier/verify.go](/service/verifier/verify.go).
```go
type verifier struct {
    verifiers atomic.Pointer[map[string]*signature.NsVerifier]
}
```

This design allows concurrent reads while supporting atomic updates.

#### Policy Updates

The `updatePolicies` method handles policy updates.
It may return error if it fails to parse the policy. This should never happen in a correct node because malformed
policies are rejected beforehand. Thus, such error should fail the node.

From [fabric-x-committer/service/verifier/verify.go](/service/verifier/verify.go).
```go
func (v *verifier) updatePolicies(
    update *protosigverifierservice.Update,
) error {
    if update == nil || (update.Config == nil && update.NamespacePolicies == nil) {
        return nil
    }
    
    // Parse new policies
    newVerifiers, err := parsePolicies(update)
    if err != nil {
        return errors.Join(ErrUpdatePolicies, err)
    }

    // Merge with existing policies
    for k, nsVerifier := range *v.verifiers.Load() {
        if _, ok := newVerifiers[k]; !ok {
            newVerifiers[k] = nsVerifier
        }
    }
    
    // Atomically update the policy map
    v.verifiers.Store(&newVerifiers)
    return nil
}
```

#### Policy Parsing

Policies can be derived from a config block (meta namespace policy), or from a namespace update (namespace policy).

From [fabric-x-committer/service/verifier/verify.go](/service/verifier/verify.go).
```go
func parsePolicies(update *protosigverifierservice.Update) (map[string]*signature.NsVerifier, error) {
    newPolicies := make(map[string]*signature.NsVerifier)
    
    // Parse config transaction policies
    if update.Config != nil {
        nsVerifier, err := policy.ParsePolicyFromConfigTx(update.Config.Envelope)
        if err != nil {
            return nil, errors.Join(ErrUpdatePolicies, err)
        }
        newPolicies[types.MetaNamespaceID] = nsVerifier
    }
    
    // Parse namespace policies
    if update.NamespacePolicies != nil {
        for _, pd := range update.NamespacePolicies.Policies {
            nsVerifier, err := policy.ParseNamespacePolicyItem(pd)
            if err != nil {
                return nil, errors.Join(ErrUpdatePolicies, err)
            }
            newPolicies[pd.Namespace] = nsVerifier
        }
    }
    return newPolicies, nil
}
```

### Verification Logic

The `verifyRequest` method verifies a transaction against namespace policies.

From [fabric-x-committer/service/verifier/verify.go](/service/verifier/verify.go).
```go
func (v *verifier) verifyRequest(request *protosigverifierservice.Request) *protosigverifierservice.Response {
    return &protosigverifierservice.Response{
        TxId:     request.Tx.Id,
        BlockNum: request.BlockNum,
        TxNum:    request.TxNum,
        Status:   v.verifyTX(request.Tx),
    }
}

func (v *verifier) verifyTX(tx *protoblocktx.Tx) protoblocktx.Status {
    if status := verifyTxForm(tx); status != retValid {
        return status
    }
	
    // The verifiers might temporarily retain the old map while updatePolicies has already set a new one.
    // This is acceptable, provided the coordinator sends the validation status to the dependency graph
    // after updating the policies in the verifier.
    // This ensures that dependent data transactions on these updated namespaces always use the map
    // containing the latest policy.
    verifiers := *v.verifiers.Load()
    for nsIndex, ns := range request.Tx.Namespaces {
        if ns.NsId == types.ConfigNamespaceID {
            // Configuration TX is not signed in the same manner as application TX.
            // Its signatures are verified by the ordering service.
            continue
        }
        nsVerifier, ok := verifiers[ns.NsId]
        if !ok {
            logger.Debugf("No verifier for namespace: '%v'", ns.NsId)
            return protoblocktx.Status_ABORTED_SIGNATURE_INVALID
        }

        // NOTE: We do not compare the namespace version in the transaction
        //       against the namespace version in the verifier. This is because if
        //       the versions mismatch, and we reject the transaction, the coordinator
        //       would mark the transaction as invalid due to a bad signature. However,
        //       this may not be true if the policy was not actually updated with the
        //       new version. Hence, we should proceed to validate the signatures. If
        //       the signatures are valid, the validator-committer service would
        //       still mark the transaction as invalid due to an MVCC conflict on the
        //       namespace version, which would reflect the correct validation status.
        if err := nsVerifier.VerifyNs(request.Tx, nsIndex); err != nil {
            logger.Debugf("Invalid signature found: '%v', namespace id: '%v'", request.Tx.Id, ns.NsId)
            return protoblocktx.Status_ABORTED_SIGNATURE_INVALID
        }
    }
	
    return retValid
}
```

The `verifyTxForm()` verifies that a transaction is well-formed. 
The `VerifyNs()` method in the `signature.NsVerifier` verifies signatures for a specific namespace:

1. It extracts the signatures for the namespace from the transaction.
2. It verifies each signature against the namespace policy.
3. It returns an error if any signature is invalid.
