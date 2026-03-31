<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Fabric-X Committer

The Fabric-X Committer implements the validation and commit stage of the Hyperledger Fabric-X transaction lifecycle. It receives ordered blocks of transactions, verifies endorsement signatures, performs MVCC (Multi-Version Concurrency Control) validation against the state database, and commits valid transactions — all through a pipelined architecture designed for high throughput.

## Architecture at a Glance

The system is built around six core services connected by gRPC streams and bounded channels:

```
Ordering Service
        │
        ▼
    Sidecar ──────────► Coordinator
                         │        │
                  Verify │        │  Validate & Commit
                         ▼        ▼
                     Verifier     VC ──► Database Cluster
                                             ▲
                                  Query ─────┘
                                 Service
```

- **Sidecar** — Fetches blocks from the Ordering Service, persists them locally, and delivers committed blocks to clients. Exposes a Notification Service for asynchronous transaction status updates.
- **Coordinator** — Orchestrates the pipeline using a transaction dependency graph to identify and dispatch conflict-free transactions for parallel processing.
- **Verifier** — Validates transaction signatures against namespace endorsement policies using a parallel worker pool.
- **Validator-Committer (VC)** — Executes a three-stage pipeline (Prepare, Validate, Commit) performing MVCC checks and committing valid transactions to the database.
- **Query Service** — Provides read-only access to the committed world state with configurable isolation levels and query batching.
- **Database Cluster** — Stores world state, transaction statuses, and namespace policies. Supports YugabyteDB (recommended) and PostgreSQL.

## Key Capabilities

- **High Throughput** — Pipelined processing with parallel dispatch of conflict-free transactions. Exceeds 100,000 TPS on commodity hardware with YugabyteDB.
- **Fault Tolerance** — Idempotent commit operations enable automatic recovery from service failures without data corruption. Each service recovers independently on restart.
- **Horizontal Scaling** — Verifier, VC, Query Service, and Database nodes scale horizontally. Sidecar and Coordinator scale vertically.
- **Flexible Endorsement Policies** — Supports both lightweight threshold rules (single public key) and fine-grained MSP rules (AND/OR/k-of-n over organizational identities).
- **Observability** — Prometheus metrics for every pipeline stage, with queue-depth gauges for bottleneck identification.

## Getting Started

See the [Setup Guide](setup.md) for prerequisites, build instructions, and running tests.

For production deployments, start with the [Deployment Guide](deployment-guide.md) for hardware sizing and topology, then the [Performance Tuning](performance-tuning.md) guide for configuration parameters.

## Documentation Overview

| Section | What You'll Find |
|---------|-----------------|
| [Architecture](architecture.md) | System design, component interactions, state management, failure recovery |
| [Deployment](deployment-guide.md) | Hardware requirements, reference topology, scaling guidance, startup order |
| [Services](sidecar.md) | Detailed documentation for each service: workflows, APIs, configuration, recovery |
| [Configuration](tls-configurations.md) | TLS setup, logging, metrics reference, namespace policies |
| [Development](core-concurrency-pattern.md) | Internal patterns (errgroup, context-aware channels) and load generator tooling |
