<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Metrics Reference

## Sidecar Metrics

The following Sidecar metrics are exported for consumption by Prometheus.

| Name                                                       | Type      | Labels                           | Description                                                                                                                                              |
|------------------------------------------------------------|-----------|----------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| sidecar_grpc_coordinator_sent_transaction_total            | counter   |                                  | Total number of transactions sent to the coordinator service.                                                                                            |
| sidecar_grpc_coordinator_received_transaction_status_total | counter   | status                           | Total number of transactions statuses received from the coordinator service.                                                                             |
| sidecar_relay_block_mapping_seconds                        | histogram |                                  | Time spent mapping a received block to an internal block.                                                                                                |
| sidecar_relay_mapped_block_processing_seconds              | histogram |                                  | Time spent processing an internal block and sending it to the coordinator.                                                                               |
| sidecar_relay_transaction_status_batch_processing_seconds  | histogram |                                  | Time spent processing a received status batch from the coordinator.                                                                                      |
| sidecar_relay_waiting_transactions_queue_size              | gauge     |                                  | Total number of transactions waiting at the relay for statuses.                                                                                          |
| sidecar_relay_output_committed_block_queue_size            | gauge     |                                  | Size of the output committed block queue of the relay service.                                                                                           |
| sidecar_notifier_input_block_queue_size                    | gauge     |                                  | Size of the committed block queue delivered from the relay to the notifier.                                                                              |
| sidecar_notifier_input_status_queue_size                   | gauge     |                                  | Size of the transaction status queue delivered from the relay to the notifier.                                                                           |
| sidecar_notifier_request_queue_size                        | gauge     |                                  | Size of the queue of notification requests received from clients.                                                                                        |
| sidecar_notifier_timeout_queue_size                        | gauge     |                                  | Size of the queue of notification requests that have timed out.                                                                                          |
| sidecar_coordinator_connection_status                      | gauge     | grpc_target                      | Connection status to coordinator service by grpc target (1 = connected, 0 = disconnected).                                                               |
| sidecar_coordinator_connection_failure_total               | counter   | grpc_target                      | Total number of connection failures to coordinator service. Short-lived failures may not always be captured.                                             |
| sidecar_grpc_requests_total                                | counter   | method                           | Number of RPCs started by the service                                                                                                                    |
| sidecar_grpc_requests_latency_seconds                      | histogram | method status                    | The latency (seconds) of requests by the service, by method and gRPC status code                                                                         |
| sidecar_grpc_stream_duration_seconds                       | histogram | method status                    | The duration (seconds) a stream was active from start to end, by method and gRPC status code                                                             |
| sidecar_grpc_active_streams                                | gauge     | method                           | Number of gRPC streams currently open on the server                                                                                                      |
| sidecar_grpc_active_connections                            | gauge     |                                  | Number of client connections currently open on the server                                                                                                |
| sidecar_ledger_append_block_seconds                        | histogram |                                  | Time spent appending a block to the ledger.                                                                                                              |
| sidecar_ledger_block_height                                | gauge     |                                  | The current block height of the ledger.                                                                                                                  |
| sidecar_relay_transaction_in_total                         | counter   |                                  | Total number of transactions received from the orderer.                                                                                                  |
| sidecar_relay_transaction_out_total                        | counter   |                                  | Total number of transaction statuses processed from the coordinator.                                                                                     |
| sidecar_notifier_pending_tx_ids                            | gauge     |                                  | Number of pending (txID, request) subscriptions waiting for status notification.                                                                         |
| sidecar_notifier_unique_pending_tx_ids                     | gauge     |                                  | Number of unique transaction IDs pending across all requests.                                                                                            |
| sidecar_notifier_tx_ids_status_deliveries_total            | counter   |                                  | Total number of transaction IDs' status deliveries to clients.                                                                                           |
| sidecar_notifier_tx_ids_timeout_deliveries_total           | counter   |                                  | Total number of transaction IDs' timeout deliveries to clients.                                                                                          |
| sidecar_delivery_data_source_id                            | gauge     |                                  | Current orderer party ID used as the data block source.                                                                                                  |
| sidecar_delivery_block_number                              | gauge     | stream_type                      | Current block number by stream type (data or headers).                                                                                                   |
| sidecar_delivery_blocks_delivered_total                    | counter   | stream_type source_id            | Total number of blocks delivered by stream type, and source ID.                                                                                          |
| sidecar_delivery_block_verification_seconds                | histogram | stream_type source_id result     | Time spent verifying blocks by stream type, source ID, and result.                                                                                       |
| sidecar_delivery_stream_restarts_total                     | counter   | reason                           | Total number of stream restarts by reason.                                                                                                               |
| sidecar_delivery_stream_starts_total                       | counter   | stream_type source_id            | Total number of stream starts by stream type and source ID.                                                                                              |
| sidecar_delivery_stream_active                             | gauge     | stream_type source_id            | Number of currently active streams by stream type, and source ID.                                                                                        |
| sidecar_delivery_stream_errors_total                       | counter   | stream_type source_id error_type | Total number of stream errors by stream type, source ID, and error type.                                                                                 |
| sidecar_delivery_withholding_suspicion_raised_total        | counter   | source_id                        | Total number of block withholding suspicions raised by data source ID.                                                                                   |
| sidecar_delivery_withholding_suspicion_cleared_total       | counter   | source_id                        | Total number of block withholding suspicions cleared by data source ID.                                                                                  |
| sidecar_delivery_withholding_suspicion_confirmed_total     | counter   | source_id                        | Total number of block withholding suspicions confirmed by data source ID.                                                                                |
| sidecar_delivery_stream_block_gap                          | gauge     | source_id                        | Current block gap between header and data streams (data - header) by data source ID.Positive value indicates that data stream is ahead of header stream. |
| sidecar_delivery_target_arrival_deadline_unix_milliseconds | gauge     | source_id                        | Unix timestamp (milliseconds) of the target block arrival deadline for withholding detection by source ID.                                               |
| sidecar_delivery_config_updates_total                      | counter   |                                  | Total number of config block updates detected.                                                                                                           |
| sidecar_delivery_config_block_number                       | gauge     |                                  | Current config block number.                                                                                                                             |
| sidecar_relay_input_block_queue_size                       | gauge     |                                  | Size of the input block queue of the relay service.                                                                                                      |
| sidecar_relay_mapped_block_queue_size                      | gauge     |                                  | Size of the relay's queue of mapped blocks waiting to be sent to the coordinator.                                                                        |
| sidecar_relay_input_status_batch_queue_size                | gauge     |                                  | Size of the relay's queue of status batches received from the coordinator.                                                                               |

## Coordinator Metrics

The following Coordinator metrics are exported for consumption by Prometheus.

| Name                                                                                   | Type      | Labels        | Description                                                                                                   |
|----------------------------------------------------------------------------------------|-----------|---------------|---------------------------------------------------------------------------------------------------------------|
| coordinator_grpc_received_transaction_total                                            | counter   |               | Total number of transactions received by the coordinator service from the client.                             |
| coordinator_grpc_committed_transaction_total                                           | counter   | status        | Total number of transactions committed status sent by the coordinator service to the client.                  |
| coordinator_grpc_requests_total                                                        | counter   | method        | Number of RPCs started by the service                                                                         |
| coordinator_grpc_requests_latency_seconds                                              | histogram | method status | The latency (seconds) of requests by the service, by method and gRPC status code                              |
| coordinator_grpc_stream_duration_seconds                                               | histogram | method status | The duration (seconds) a stream was active from start to end, by method and gRPC status code                  |
| coordinator_grpc_active_streams                                                        | gauge     | method        | Number of gRPC streams currently open on the server                                                           |
| coordinator_grpc_active_connections                                                    | gauge     |               | Number of client connections currently open on the server                                                     |
| coordinator_verifier_connection_status                                                 | gauge     | grpc_target   | Connection status to verifier service by grpc target (1 = connected, 0 = disconnected).                       |
| coordinator_verifier_connection_failure_total                                          | counter   | grpc_target   | Total number of connection failures to verifier service. Short-lived failures may not always be captured.     |
| coordinator_verifier_transaction_processed_total                                       | counter   |               | Total number of transactions processed by the manager.                                                        |
| coordinator_verifier_transaction_retried_total                                         | counter   |               | Total number of transactions retried by the manager.                                                          |
| coordinator_verifier_input_batch_queue_size                                            | gauge     |               | Size of the input batch queue of the manager.                                                                 |
| coordinator_verifier_output_batch_queue_size                                           | gauge     |               | Size of the output batch queue of the manager.                                                                |
| coordinator_vcservice_connection_status                                                | gauge     | grpc_target   | Connection status to vcservice service by grpc target (1 = connected, 0 = disconnected).                      |
| coordinator_vcservice_connection_failure_total                                         | counter   | grpc_target   | Total number of connection failures to vcservice service. Short-lived failures may not always be captured.    |
| coordinator_vcservice_transaction_processed_total                                      | counter   |               | Total number of transactions processed by the manager.                                                        |
| coordinator_vcservice_transaction_retried_total                                        | counter   |               | Total number of transactions retried by the manager.                                                          |
| coordinator_vcservice_input_batch_queue_size                                           | gauge     |               | Size of the input batch queue of the manager.                                                                 |
| coordinator_vcservice_output_batch_queue_size                                          | gauge     |               | Size of the output batch queue of the manager.                                                                |
| coordinator_vcservice_output_tx_status_batch_queue_size                                | gauge     |               | Size of the output transaction status batch queue of the validation and committer service manager.            |
| coordinator_local_dependency_graph_input_tx_batch_queue_size                           | gauge     |               | Size of the input transaction batch queue of the local dependency graph manager                               |
| coordinator_global_dependency_graph_input_tx_batch_queue_size                          | gauge     |               | Size of the input transaction batch queue of the global dependency graph manager                              |
| coordinator_global_dependency_graph_size                                               | gauge     |               | Size of the global dependency graph manager in terms of the number of transactions waiting to be processed    |
| coordinator_local_dependency_graph_tx_processed_total                                  | counter   |               | Total number of new transactions processed by the local dependency graph manager                              |
| coordinator_global_dependency_graph_tx_processed_total                                 | counter   |               | Total number of new transactions processed by the global dependency graph manager                             |
| coordinator_global_dependency_graph_validated_tx_processed_total                       | counter   |               | Total number of validated transactions processed by the global dependency graph manager                       |
| coordinator_global_dependency_graph_construction_seconds                               | histogram |               | Time spent adding a transaction batch to the global dependency graph in the global dependency graph manager   |
| coordinator_global_dependency_graph_constructor_wait_for_lock_seconds                  | histogram |               | Time spent waiting for the lock in the constructor of the global dependency graph manager                     |
| coordinator_global_dependency_graph_add_tx_batch_to_graph_seconds                      | histogram |               | Time spent adding a transaction batch to the graph in the global dependency graph manager                     |
| coordinator_global_dependency_graph_update_dependency_detector_seconds                 | histogram |               | Time spent updating the dependency detector with a transaction batch in the global dependency graph manager   |
| coordinator_global_dependency_graph_validated_tx_batch_processing_seconds              | histogram |               | Time spent processing a validated transaction batch in the global dependency graph manager                    |
| coordinator_global_dependency_graph_validated_tx_batch_processor_wait_for_lock_seconds | histogram |               | Time spent waiting for the lock in the validated transaction processor of the global dependency graph manager |
| coordinator_global_dependency_graph_remove_dependents_of_validated_tx_batch_seconds    | histogram |               | Time spent removing the dependents of a validated transaction batch in the global dependency graph manager    |
| coordinator_global_dependency_graph_add_freed_tx_batch_seconds                         | histogram |               | Time spent adding a freed transaction batch to a queue in the global dependency graph manager                 |
| coordinator_global_dependency_graph_output_freed_tx_batch_seconds                      | histogram |               | Time spent outputting a freed transaction batch in the global dependency graph manager                        |
| coordinator_dependency_graph_dependent_transactions_queue_size                         | gauge     |               | The number of transactions currently waiting on dependencies.                                                 |

## Verifier Metrics

The following Verifier metrics are exported for consumption by Prometheus.

| Name                                              | Type      | Labels        | Description                                                                                  |
|---------------------------------------------------|-----------|---------------|----------------------------------------------------------------------------------------------|
| verifier_server_tx_input_throughput               | counter   |               | Incoming requests for a component                                                            |
| verifier_server_tx_output_throughput              | counter   |               | Outgoing responses for a component                                                           |
| verifier_server_grpc_requests_total               | counter   | method        | Number of RPCs started by the service                                                        |
| verifier_server_grpc_requests_latency_seconds     | histogram | method status | The latency (seconds) of requests by the service, by method and gRPC status code             |
| verifier_server_grpc_stream_duration_seconds      | histogram | method status | The duration (seconds) a stream was active from start to end, by method and gRPC status code |
| verifier_server_grpc_active_streams               | gauge     | method        | Number of gRPC streams currently open on the server                                          |
| verifier_server_grpc_active_connections           | gauge     |               | Number of client connections currently open on the server                                    |
| verifier_server_parallel_executor_active_requests | gauge     |               | The total number of active requests                                                          |

## Validator-Committer Metrics

The following Validator-Committer metrics are exported for consumption by Prometheus.

| Name                                                                         | Type      | Labels        | Description                                                                                                 |
|------------------------------------------------------------------------------|-----------|---------------|-------------------------------------------------------------------------------------------------------------|
| vcservice_grpc_received_transaction_total                                    | counter   |               | Number of transactions received by the service                                                              |
| vcservice_grpc_processed_transaction_total                                   | counter   |               | Number of transactions processed by the service                                                             |
| vcservice_grpc_requests_total                                                | counter   | method        | Number of RPCs started by the service                                                                       |
| vcservice_grpc_requests_latency_seconds                                      | histogram | method status | The latency (seconds) of requests by the service, by method and gRPC status code                            |
| vcservice_grpc_stream_duration_seconds                                       | histogram | method status | The duration (seconds) a stream was active from start to end, by method and gRPC status code                |
| vcservice_grpc_active_streams                                                | gauge     | method        | Number of gRPC streams currently open on the server                                                         |
| vcservice_grpc_active_connections                                            | gauge     |               | Number of client connections currently open on the server                                                   |
| vcservice_committed_transaction_total                                        | counter   |               | The total number of transactions committed                                                                  |
| vcservice_mvcc_conflict_total                                                | counter   |               | The total number of transactions that failed due to MVCC conflict                                           |
| vcservice_duplicate_transaction_total                                        | counter   |               | The total number of duplicate transactions                                                                  |
| vcservice_preparer_input_queue_size                                          | gauge     |               | The preparer input queue size                                                                               |
| vcservice_validator_input_queue_size                                         | gauge     |               | The validator input queue size                                                                              |
| vcservice_committer_input_queue_size                                         | gauge     |               | The committer input queue size                                                                              |
| vcservice_txstatus_output_queue_size                                         | gauge     |               | The txstatus output queue size                                                                              |
| vcservice_preparer_tx_batch_latency_seconds                                  | histogram |               | The latency of the preparer processing a batch of transactions                                              |
| vcservice_validator_tx_batch_latency_seconds                                 | histogram |               | The latency of the validator processing a batch of transactions                                             |
| vcservice_committer_tx_batch_latency_seconds                                 | histogram |               | The latency of the committer processing a batch of transactions                                             |
| vcservice_database_tx_batch_validation_latency_seconds                       | histogram |               | The latency of the database validating a batch of transactions                                              |
| vcservice_database_tx_batch_query_version_latency_seconds                    | histogram |               | The latency of the database querying version for keys in a batch of transactions                            |
| vcservice_database_tx_batch_commit_latency_seconds                           | histogram |               | The latency of the database committing a batch of transactions                                              |
| vcservice_database_tx_batch_commit_txs_status_latency_seconds                | histogram |               | The latency of the database committing a batch of transactions and updating their status                    |
| vcservice_database_tx_batch_commit_update_latency_seconds                    | histogram |               | The latency of the database committing a batch of transactions which involes updating existing keys         |
| vcservice_database_tx_batch_commit_insert_new_key_with_value_latency_seconds | histogram |               | The latency of the database committing a batch of transactions which involes inserting new keys with values |

## Query Service Metrics

The following Query Service metrics are exported for consumption by Prometheus.

| Name                                                     | Type      | Labels        | Description                                                                                  |
|----------------------------------------------------------|-----------|---------------|----------------------------------------------------------------------------------------------|
| queryservice_grpc_requests_total                         | counter   | method        | Number of RPCs started by the service                                                        |
| queryservice_grpc_requests_latency_seconds               | histogram | method status | The latency (seconds) of requests by the service, by method and gRPC status code             |
| queryservice_grpc_stream_duration_seconds                | histogram | method status | The duration (seconds) a stream was active from start to end, by method and gRPC status code |
| queryservice_grpc_active_streams                         | gauge     | method        | Number of gRPC streams currently open on the server                                          |
| queryservice_grpc_active_connections                     | gauge     |               | Number of client connections currently open on the server                                    |
| queryservice_grpc_key_requested_total                    | counter   |               | Number of keys requested by the service                                                      |
| queryservice_grpc_key_responded_total                    | counter   |               | Number of keys responded by the service                                                      |
| queryservice_database_processing_sessions                | gauge     | session       | Number of processing sessions in the service                                                 |
| queryservice_database_batch_queueing_time_seconds        | histogram |               | The time batches waits for execution                                                         |
| queryservice_database_batch_query_size                   | histogram |               | The size of submitted batches                                                                |
| queryservice_database_batch_response_size                | histogram |               | The size of response for batch queries                                                       |
| queryservice_database_request_assignment_latency_seconds | histogram |               | The latency of the query request assignment to the queue                                     |
| queryservice_database_query_latency_seconds              | histogram |               | The latency of the queries' batches                                                          |

## Load Generator Metrics

The following Load Generator metrics are exported for consumption by Prometheus.

| Name                                        | Type      | Labels | Description                                                                                                                                                                                   |
|---------------------------------------------|-----------|--------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| loadgen_block_sent_total                    | counter   |        | Total number of blocks sent by the block generator                                                                                                                                            |
| loadgen_block_received_total                | counter   |        | Total number of blocks received by the block generator                                                                                                                                        |
| loadgen_transaction_sent_total              | counter   |        | Total number of transactions sent by the block generator                                                                                                                                      |
| loadgen_transaction_received_total          | counter   |        | Total number of transactions received by the block generator                                                                                                                                  |
| loadgen_transaction_committed_total         | counter   |        | Total number of transaction commit statuses received by the block generator                                                                                                                   |
| loadgen_transaction_aborted_total           | counter   |        | Total number of transaction abort statuses received by the block generator                                                                                                                    |
| loadgen_valid_transaction_latency_seconds   | histogram |        | Latency of valid transactions in seconds                                                                                                                                                      |
| loadgen_invalid_transaction_latency_seconds | histogram |        | Latency of invalid transactions in seconds                                                                                                                                                    |
| loadgen_created_keys_total                  | counter   |        | Total number of new keys the workload has introduced, each counted once when it is first created; some may never be committed (for example, when the transaction that creates the key aborts) |
| loadgen_referenced_read_keys_total          | counter   |        | Total number of read-only accesses that reused an already-created key instead of introducing a new one                                                                                        |
| loadgen_referenced_write_keys_total         | counter   |        | Total number of write accesses (read-write and blind-write) that reused an already-created key instead of introducing a new one                                                               |

---
