<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Metrics Reference

## Sidecar Metrics

The following Sidecar metrics are exported for consumption by Prometheus.

| Name | Type | Labels | Description |
| ---- | ---- | ------ | ----------- |
| sidecar_grpc_coordinator_sent_transaction_total | counter |  | Total number of transactions sent to the coordinator service. |
| sidecar_grpc_coordinator_received_transaction_status_total | counter | status | Total number of transactions statuses received from the coordinator service. |
| sidecar_relay_block_mapping_seconds | histogram |  | Time spent mapping a received block to an internal block. |
| sidecar_relay_mapped_block_processing_seconds | histogram |  | Time spent processing an internal block and sending it to the coordinator. |
| sidecar_relay_transaction_status_batch_processing_seconds | histogram |  | Time spent processing a received status batch from the coordinator. |
| sidecar_relay_waiting_transactions_queue_size | gauge |  | Total number of transactions waiting at the relay for statuses. |
| sidecar_relay_input_block_queue_size | gauge |  | Size of the input block queue of the relay service. |
| sidecar_relay_output_committed_block_queue_size | gauge |  | Size of the output committed block queue of the relay service. |
| sidecar_ledger_append_block_seconds | histogram |  | Time spent appending a block to the ledger. |
| sidecar_ledger_block_height | gauge |  | The current block height of the ledger. |

## Coordinator Metrics

The following Coordinator metrics are exported for consumption by Prometheus.

| Name | Type | Labels | Description |
| ---- | ---- | ------ | ----------- |
| coordinator_grpc_received_transaction_total | counter |  | Total number of transactions received by the coordinator service from the client. |
| coordinator_grpc_committed_transaction_total | counter | status | Total number of transactions committed status sent by the coordinator service to the client. |
| coordinator_sigverifier_input_tx_batch_queue_size | gauge |  | Size of the input transaction batch queue of the signature verifier manager. |
| coordinator_sigverifier_output_validated_tx_batch_queue_size | gauge |  | Size of the output validated transaction batch queue of the signature verifier manager. |
| coordinator_vcservice_output_tx_status_batch_queue_size | gauge |  | Size of the output transaction status batch queue of the validation and committer service manager. |
| coordinator_vcservice_output_validated_tx_batch_queue_size | gauge |  | Size of the output validated transaction batch queue of the validation and committer service manager. |
| coordinator_sigverifier_transaction_processed_total | counter |  | Total number of transactions processed by the signature verifier manager. |
| coordinator_vcservice_transaction_processed_total | counter |  | Total number of transactions processed by the validation and committer service manager. |
| coordinator_vcservice_retired_transaction_total | counter |  | Total number of transactions retried by the validation and committer service manager. |
| coordinator_sigverifier_retired_transaction_total | counter |  | Total number of transactions retried by the signature verifier manager. |

## Verifier Metrics

The following Verifier metrics are exported for consumption by Prometheus.

| Name | Type | Labels | Description |
| ---- | ---- | ------ | ----------- |
| verifier_server_grpc_active_streams | gauge |  | The total number of started streams |
| verifier_server_parallel_executor_active_requests | gauge |  | The total number of active requests |

## Validator-Committer Metrics

The following Validator-Committer metrics are exported for consumption by Prometheus.

| Name | Type | Labels | Description |
| ---- | ---- | ------ | ----------- |
| vcservice_grpc_received_transaction_total | counter |  | Number of transactions received by the service |
| vcservice_grpc_processed_transaction_total | counter |  | Number of transactions processed by the service |
| vcservice_committed_transaction_total | counter |  | The total number of transactions committed |
| vcservice_mvcc_conflict_total | counter |  | The total number of transactions that failed due to MVCC conflict |
| vcservice_duplicate_transaction_total | counter |  | The total number of duplicate transactions |
| vcservice_preparer_input_queue_size | gauge |  | The preparer input queue size |
| vcservice_validator_input_queue_size | gauge |  | The validator input queue size |
| vcservice_committer_input_queue_size | gauge |  | The committer input queue size |
| vcservice_txstatus_output_queue_size | gauge |  | The txstatus output queue size |
| vcservice_preparer_tx_batch_latency_seconds | histogram |  | The latency of the preparer processing a batch of transactions |
| vcservice_validator_tx_batch_latency_seconds | histogram |  | The latency of the validator processing a batch of transactions |
| vcservice_committer_tx_batch_latency_seconds | histogram |  | The latency of the committer processing a batch of transactions |
| vcservice_database_tx_batch_validation_latency_seconds | histogram |  | The latency of the database validating a batch of transactions |
| vcservice_database_tx_batch_query_version_latency_seconds | histogram |  | The latency of the database querying version for keys in a batch of transactions |
| vcservice_database_tx_batch_commit_latency_seconds | histogram |  | The latency of the database committing a batch of transactions |
| vcservice_database_tx_batch_commit_txs_status_latency_seconds | histogram |  | The latency of the database committing a batch of transactions and updating their status |
| vcservice_database_tx_batch_commit_update_latency_seconds | histogram |  | The latency of the database committing a batch of transactions which involes updating existing keys |
| vcservice_database_tx_batch_commit_insert_new_key_with_value_latency_seconds | histogram |  | The latency of the database committing a batch of transactions which involes inserting new keys with values |
| vcservice_database_tx_batch_commit_insert_new_key_without_value_latency_seconds | histogram |  | The latency of the database committing a batch of transactions which involes inserting new keys without values |

## Query Service Metrics

The following Query Service metrics are exported for consumption by Prometheus.

| Name | Type | Labels | Description |
| ---- | ---- | ------ | ----------- |
| queryservice_grpc_requests_total | counter | method | Number of requests by the service |
| queryservice_grpc_requests_latency_seconds | histogram | method | The latency (seconds) of requests by the service |
| queryservice_grpc_key_requested_total | counter |  | Number of keys requested by the service |
| queryservice_grpc_key_responded_total | counter |  | Number of keys responded by the service |
| queryservice_database_processing_sessions | gauge | session | Number of processing sessions in the service |
| queryservice_database_batch_queueing_time_seconds | histogram |  | The time batches waits for execution |
| queryservice_database_batch_query_size | histogram |  | The size of submitted batches |
| queryservice_database_batch_response_size | histogram |  | The size of response for batch queries |
| queryservice_database_request_assignment_latency_seconds | histogram |  | The latency of the query request assignment to the queue |
| queryservice_database_query_latency_seconds | histogram |  | The latency of the queries' batches |

---
