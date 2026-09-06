<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Snapshot Hasher

1. [Overview](#1-overview)
2. [Why a Separate Service](#2-why-a-separate-service)
3. [Scheduling Model](#3-scheduling-model)
4. [Snapshot Record Lifecycle](#4-snapshot-record-lifecycle)
5. [Hash Computation](#5-hash-computation)
6. [Configuration](#6-configuration)
7. [Failure and Recovery](#7-failure-and-recovery)

## 1. Overview

The snapshot hasher turns a committed `_snapshot` record into a content hash of the
snapshot's clone database. It is the only component that hashes snapshots. It is named
for what it does, and deliberately not "snapshot service": it never creates a snapshot
or its clone — the validator-committer does that on the commit path.

It exposes no RPCs of its own. Work reaches it exclusively through the state
database: the validator-committer (VC) commits a `_snapshot` record together with its
clone, and this service discovers that record on a later poll. Its gRPC server exists
only so the service can be health-checked the same way as every other service.

Implementation: [service/snapshothasher](/service/snapshothasher). The durable record contract it
shares with the VC lives in [utils/snapshotstate](/utils/snapshotstate).

## 2. Why a Separate Service

Hashing a snapshot is a full scan of every hashed table in a clone. It is neither part
of committing a transaction nor something that several processes should attempt at
once, so it is deployed as **one instance** alongside any number of VCs.

That single-instance deployment is what keeps the design simple. With exactly one
scheduler in the system, and hashing running inline on its polling goroutine, one
snapshot is hashed at a time and no cross-process exclusion is needed — no lease, no
ownership token, no leader election. Running a second instance is a deployment error;
because a clone is immutable and the digest is deterministic, the symptom is duplicate
work writing the same digest, not a corrupted record.

The VC's only snapshot duty is therefore to make the record durable atomically with
its clone. See [Creating State Snapshots](validator-committer.md#task-4-creating-state-snapshots-clone-first).

## 3. Scheduling Model

The hasher polls the latest `_snapshot` record every `poll-interval` and hashes it
whenever it still needs hashing. Nothing notifies it, so:

- hashing begins within one interval of the snapshot committing;
- after a restart, the first check happens one interval in;
- one path covers a fresh snapshot, one the coordinator resubmitted, and one orphaned
  mid-hash by a restart — they differ only in the status the tick reads.

Hashing runs inline, so a job that outlives the interval delays the next check rather
than starting a second hash.

The latest-snapshot pointer in the `metadata` table is what makes discovery a single
key lookup instead of a scan of the growing `ns__snapshot` table. The VC writes that
pointer in the same transaction as the record it names, so the pointer never names a
row that did not commit.

## 4. Snapshot Record Lifecycle

| Status read by a tick | Action |
|---|---|
| `PENDING` | Hash it: this is a snapshot the VC just committed |
| `IN_PROGRESS` | Hash it: the only way this is seen is a job orphaned by a restart |
| `FAILED` | Hash it again: a clone is immutable, so a retry cannot produce a different digest |
| `COMPLETED` | Leave untouched — the digest is already published |
| `CHECKPOINTED` | Leave untouched — re-hashing could only undo the checkpoint |

Two conditions stop the service instead of being retried:

- **A committed record whose clone is not there to hash.** The clone is created before
  its snapshot transaction commits, so a committed record always names a clone that
  exists. Neither shape of absence — no `clone_database` recorded, or a recorded name
  whose database is gone — can be produced by this system, which leaves external
  interference or storage corruption. No retry repairs either, and recording a failed
  attempt on the record would invite a later tick to treat it as retryable work, so the
  service stops and leaves the record as evidence.
- **A record with no `TxRef`** is a hard error, because a record that cannot be named
  cannot be driven; treating it as "nothing to do" would stall hashing silently.

A missing clone database is reported on the first attempt rather than after the
database retry budget: `statedb.NewPool` treats SQLSTATE 3D000 (`invalid_catalog_name`)
as terminal, since a database that does not exist will not appear on a later attempt.

A failed hash records its cause on the record itself, so an operator sees why without
reading this hasher's log, and the next tick retries it.

## 5. Hash Computation

The digest is a SHA-256 over the clone's committed content, computed so that identical
clone content always yields an identical digest:

- The hashed set is derived from `ns__meta`, the authoritative namespace registry:
  every user namespace's `ns_<id>` table, plus `ns__meta`, `ns__config`, and
  `tx_status`. `metadata`, `ns__snapshot`, and `ns__checkpoint` are excluded — the
  first two are exactly the tables this service and the VC write while a snapshot is in
  flight, so including them would make the digest depend on hashing progress.
- Each table is scanned in primary-key order in bounded pages (keyset pagination), so
  worker memory stays bounded on large tables and the scan is served by the primary-key
  index with no sort step.
- Rows are folded in with length-prefixed encoding (`len(key)||key||len(value)||value`),
  which prevents boundary collisions between adjacent keys and values.
- Per-table digests are combined in sorted table-name order, so the result does not
  depend on which table finished first.

Only the combined root hash is persisted today. Localizing a divergence between
organizations — per-table digests, then a Merkle Patricia trie over a table's rows — is
planned work for this service and does not change this encoding. That work is still
hashing, only at a finer granularity, so it fits the service's name rather than
outgrowing it.

## 6. Configuration

Sample: [cmd/config/samples/snapshot-hasher.yaml](/cmd/config/samples/snapshot-hasher.yaml).

| Field | Default | Meaning |
|---|---|---|
| `database` | — | State-database connection. `max-connections` sizes only the record poll (one statement at a time); each clone gets its own short-lived pool instead, sized from `max-workers-for-hash` |
| `poll-interval` | `1m` | How often the latest snapshot record is re-read; also the scheduling latency and the restart resume delay |
| `resource-limits.max-workers-for-hash` | `4` | Tables hashed in parallel within one job, and therefore the clone pool's connection count |
| `resource-limits.hash-batch-size` | `1000` | Rows fetched per round-trip while scanning a table |

Tuning guidance: [Performance Tuning — Snapshot Hasher](performance-tuning.md#8-snapshot-hasher).

Start it with:

```bash
./bin/committer start snapshot-hasher --config <config-file>
```

## 7. Failure and Recovery

The hasher holds no state of its own; the `_snapshot` record is the state.

- **Restart mid-hash.** The record is left `IN_PROGRESS` and the partial digest is
  discarded. The first tick after restart re-reads that record and hashes it from
  scratch. Because the clone is immutable, the recomputed digest is the same one the
  interrupted attempt would have produced.
- **Transient database or clone failure.** The tick logs the failure and records it on
  the record; the loop keeps running and the next tick retries. A failure never stops
  the service, because the durable record is still there to be picked up.
- **Hasher down entirely.** Ordinary transaction commit is unaffected — the VC never
  calls this service and never waits on it. Only snapshotting stalls: the outstanding
  record is not hashed, so it never reaches `CHECKPOINTED`, and the VC rejects further
  snapshot requests until the hasher returns and drives that record to `COMPLETED`. So
  at most one snapshot is outstanding while the hasher is down, not a backlog of them.
- **A record the hasher refuses to drive.** The service exits, so a supervisor's restart
  loop and the exit message are the signal. Because checkpointing proceeds only from a
  `COMPLETED` record, the snapshot stalls rather than being checkpointed without a
  digest, and the VC keeps rejecting new snapshots until the latest one is
  `CHECKPOINTED` — so this needs an operator, and the exit message names the record and
  the clone it could not hash.

### Observability

`snapshothasher_poll_errors_total` is the metric to alert on: it counts ticks that
could not determine whether there is work at all (state database unreachable, a
pointer that names no row, a record that does not decode). The hash-job counters
cannot express those — such a tick completes no job and fails none — so without it a
service whose database is down looks the same as an idle one.

`snapshothasher_hash_in_progress` and `snapshothasher_hash_started_timestamp_seconds`
cover the opposite blind spot. Hashing a clone is a full scan and can run for many
minutes, but `snapshothasher_hash_duration_seconds` is only observed once a hash
returns, so while one is running a busy service and an idle one publish identical
numbers. Alert on the elapsed time rather than the boolean, since a stuck job holds the
boolean at 1 and never reaches the histogram:

```promql
# Hashing has been running for more than an hour.
snapshothasher_hash_in_progress == 1
  and (time() - snapshothasher_hash_started_timestamp_seconds) > 3600
```

See [Metrics Reference](metrics_reference.md).
