<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Namespace Policy

1. [Overview](#1-overview)
    - [What Are Namespace Policies?](#what-are-namespace-policies)
    - [Fabric vs Fabric-X Comparison](#fabric-vs-fabric-x-comparison)
2. [Endorsement Structure](#2-endorsement-structure)
3. [Threshold Rules](#3-threshold-rules)
    - [Supported Signature Schemes](#supported-signature-schemes)
    - [Example: ECDSA Threshold Rule](#example-ecdsa-threshold-rule)
4. [Fine-Grained Policies (MSP Rules)](#4-fine-grained-policies-msp-rules)
    - [MSP Principals](#msp-principals)
    - [Policy Combinators](#policy-combinators)
    - [Examples](#examples)
5. [Cached Identities](#5-cached-identities)
    - [Wire Format](#wire-format)
    - [Provisioning](#provisioning)
6. [Policy Lifecycle](#6-policy-lifecycle)
    - [Creating Namespace Policies](#creating-namespace-policies)
    - [Namespace ID Constraints](#namespace-id-constraints)

## 1. Overview

### What Are Namespace Policies?

A namespace in Fabric-X is the unit of state isolation — analogous to a chaincode in Hyperledger Fabric.
Every namespace must have a **namespace policy** that defines which identities are authorized to endorse
state changes within that namespace. No transaction can modify a namespace's state without satisfying its policy.

Namespace policies serve the same role as **endorsement policies** in Fabric: they specify the set of
signatures required for a transaction to be considered valid at commit time.

Fabric-X supports two types of namespace policies:

1. **Threshold Rule** — A single signer identified by a raw public key. Fast, lightweight, no MSP infrastructure required.
2. **MSP Rule** — A fine-grained policy using AND/OR/NOutOf combinators over MSP principals. Compatible with Fabric's `SignaturePolicyEnvelope`.

### Fabric vs Fabric-X Comparison

| Concept                | Hyperledger Fabric                                         | Fabric-X                                              |
|------------------------|------------------------------------------------------------|-------------------------------------------------------|
| State isolation unit   | Chaincode                                                  | Namespace                                             |
| Policy binding         | Per chaincode, per collection, or per key                  | Per namespace                                         |
| Simple policy          | Not available — all policies use `SignaturePolicyEnvelope`  | **Threshold Rule**: raw public key + scheme           |
| Complex policy         | `SignaturePolicyEnvelope` with AND/OR/OutOf                | **MSP Rule**: same `SignaturePolicyEnvelope` structure |
| Channel-level policy   | ImplicitMeta (e.g., `MAJORITY Endorsement`)                | Same — used for lifecycle operations only             |
| Identity in endorsement| Full X.509 certificate                                     | Full certificate **or** cached identity ID            |
| Policy location        | Chaincode definition or key-level metadata                 | `_meta` namespace transactions                        |

**Key difference:** Fabric requires the full MSP infrastructure even for single-signer namespaces.
Fabric-X adds the **Threshold Rule** as a lightweight alternative — a raw public key is sufficient,
bypassing certificate parsing, MSP deserialization, and policy evaluation entirely.

## 2. Endorsement Structure

Every transaction carries one `Endorsements` message per namespace it touches, and each contains a list
of individual `EndorsementWithIdentity` entries:

```protobuf
message Endorsements {
    repeated EndorsementWithIdentity endorsements_with_identity = 1;
}

message EndorsementWithIdentity {
    bytes endorsement = 1;    // The raw cryptographic signature bytes
    Identity identity = 2;    // Who produced this signature
}

message Identity {
    string msp_id = 1;        // MSP identifier (e.g., "Org1MSP")
    oneof creator {
        bytes certificate = 2;     // Full X.509 certificate bytes
        string certificate_id = 3; // Cached identity ID (see §5)
    }
}
```

The two policy types use `EndorsementWithIdentity` differently:

| Field                  | Threshold Rule                                                       | MSP Rule                                                                     |
|------------------------|----------------------------------------------------------------------|------------------------------------------------------------------------------|
| `endorsement`          | ECDSA/BLS/EdDSA signature over namespace data                       | Same — signature over namespace data                                         |
| `identity`             | **Ignored** — the public key is in the policy itself                 | **Required** — must carry a full certificate or cached ID                    |
| Entries per namespace  | Exactly 1                                                            | One per endorsing organization that signed                                   |

**Why this design:** Threshold rules identify the signer through the policy (a single known public key),
so the endorsement only needs the signature bytes. MSP rules must match each signature to an MSP
principal, so each endorsement must carry the signer's identity for the MSP to deserialize and validate.

## 3. Threshold Rules

A threshold rule defines a namespace policy with a **single signer** identified by a raw public key and
a cryptographic scheme. This is the simplest and fastest policy type — the verifier checks one signature
against one known public key with no MSP overhead.

**When to use:**

- Single-organization namespaces where one entity controls all state changes
- IoT or device signing scenarios with pre-provisioned keys
- Performance-critical paths where MSP deserialization overhead is unacceptable

**Protobuf structure:**

```protobuf
message NamespacePolicy {
    oneof Rule {
        ThresholdRule threshold_rule = 1;
        bytes msp_rule = 2;
    }
}

message ThresholdRule {
    string scheme = 1;    // "ECDSA", "BLS", or "EDDSA"
    bytes public_key = 2; // Raw public key bytes
}
```

### Supported Signature Schemes

| Scheme    | Key Format                     | Signature Format               | Notes                                                                       |
|-----------|--------------------------------|--------------------------------|-----------------------------------------------------------------------------|
| **ECDSA** | PEM-encoded EC public key      | ASN.1 DER-encoded (r, s)      | Standard choice. Compatible with existing PKI and X.509 certificates.       |
| **BLS**   | bn254 curve point              | bn254 pairing signature        | Useful for aggregate signatures. Uses `consensys/gnark-crypto`.             |
| **EdDSA** | Ed25519 public key (32 bytes)  | Ed25519 signature (64 bytes)   | Fast verification, deterministic signatures, fixed-size keys.               |

ECDSA is the recommended default. BLS and EdDSA are available for specialized use cases
(aggregate signatures and high-throughput verification, respectively).

### Example: ECDSA Threshold Rule

Creating a namespace policy with an ECDSA threshold rule:

```go
import (
    "github.com/hyperledger/fabric-x-common/api/applicationpb"
    "github.com/hyperledger/fabric-x-committer/utils/signature"
)

policy := &applicationpb.NamespacePolicy{
    Rule: &applicationpb.NamespacePolicy_ThresholdRule{
        ThresholdRule: &applicationpb.ThresholdRule{
            Scheme:    signature.Ecdsa,
            PublicKey: ecdsaPublicKeyPEM,  // PEM-encoded ECDSA public key
        },
    },
}
```

**Limitation:** Threshold rules support exactly one signer. If your namespace requires endorsement from
multiple parties (e.g., "both Org1 and Org2 must sign"), use a fine-grained MSP rule instead.

## 4. Fine-Grained Policies (MSP Rules)

MSP rules provide multi-party endorsement policies using Fabric's `SignaturePolicyEnvelope` structure.
They support AND, OR, and k-of-n threshold combinators over MSP principals, enabling complex
governance policies like "2 out of 3 organizations must endorse" or "Org1's admin AND any member of Org2".

**When to use:**

- Multi-organization namespaces requiring endorsement from multiple parties
- Regulatory or compliance scenarios requiring specific signer combinations
- Complex governance structures with nested conditions

### MSP Principals

An MSP principal defines an identity requirement — a constraint that an endorser's identity must satisfy.

| Principal Type           | Description                                                | Example                            |
|--------------------------|------------------------------------------------------------|------------------------------------|
| **ROLE**                 | Identity must belong to a specific MSP with a specific role| `Org1MSP.MEMBER`, `Org2MSP.ADMIN`  |
| **ORGANIZATION_UNIT**    | Identity must have a specific organizational unit          | `Org1MSP.client` (via NodeOUs)     |
| **IDENTITY**             | Identity must match a specific certificate exactly         | Exact cert match                   |
| **COMBINED**             | Identity must satisfy ALL listed principals simultaneously | `Org1MSP.ADMIN AND Org1MSP.client` |

**Supported roles:**

| Role      | Description                                  |
|-----------|----------------------------------------------|
| `MEMBER`  | Any valid identity in the MSP                |
| `ADMIN`   | Administrative identity                      |
| `CLIENT`  | Client identity (requires NodeOUs enabled)   |
| `PEER`    | Peer identity (requires NodeOUs enabled)     |
| `ORDERER` | Orderer identity (requires NodeOUs enabled)  |

### Policy Combinators

The policy tree is built from two node types:

- **`SignedBy(index)`** — Leaf node. Requires a valid signature from an identity that satisfies the
  principal at position `index` in the identities array.
- **`NOutOf(n, rules)`** — Interior node. Requires at least `n` of its child rules to be satisfied.

AND and OR are syntactic sugar over NOutOf:

| Expression          | Equivalent NOutOf         | Meaning                                    |
|---------------------|---------------------------|--------------------------------------------|
| `AND(A, B)`         | `NOutOf(2, [A, B])`      | Both A and B must be satisfied             |
| `OR(A, B)`          | `NOutOf(1, [A, B])`      | At least one of A or B must be satisfied   |
| `OutOf(2, A, B, C)` | `NOutOf(2, [A, B, C])`   | At least 2 of the 3 must be satisfied      |

The `SignaturePolicyEnvelope` protobuf separates the identity definitions (flat array) from the
policy logic (recursive tree). Leaf nodes reference identities by index:

```protobuf
message SignaturePolicyEnvelope {
    int32 version = 1;
    SignaturePolicy rule = 2;               // Recursive policy tree
    repeated MSPPrincipal identities = 3;   // Flat array of principals
}

message SignaturePolicy {
    oneof Type {
        int32 signed_by = 1;        // Index into identities array
        NOutOf n_out_of = 2;        // Requires N child rules satisfied
    }
    message NOutOf {
        int32 n = 1;
        repeated SignaturePolicy rules = 2;
    }
}
```

### Examples

**AND — Both organizations must endorse:**

```
Policy: AND('Org1MSP.member', 'Org2MSP.member')

identities: [
    MSPPrincipal{ROLE, {msp: "Org1MSP", role: MEMBER}},  // index 0
    MSPPrincipal{ROLE, {msp: "Org2MSP", role: MEMBER}},  // index 1
]
rule: NOutOf(2, [SignedBy(0), SignedBy(1)])
```

**OR — Either organization's admin suffices:**

```
Policy: OR('Org1MSP.admin', 'Org2MSP.admin')

identities: [
    MSPPrincipal{ROLE, {msp: "Org1MSP", role: ADMIN}},   // index 0
    MSPPrincipal{ROLE, {msp: "Org2MSP", role: ADMIN}},   // index 1
]
rule: NOutOf(1, [SignedBy(0), SignedBy(1)])
```

**Threshold — 2 out of 3:**

```
Policy: OutOf(2, 'Org1MSP.member', 'Org2MSP.member', 'Org3MSP.member')

identities: [
    MSPPrincipal{ROLE, {msp: "Org1MSP", role: MEMBER}},  // index 0
    MSPPrincipal{ROLE, {msp: "Org2MSP", role: MEMBER}},  // index 1
    MSPPrincipal{ROLE, {msp: "Org3MSP", role: MEMBER}},  // index 2
]
rule: NOutOf(2, [SignedBy(0), SignedBy(1), SignedBy(2)])
```

**Nested — Complex governance:**

```
Policy: AND(OR('Org1MSP.admin', 'Org2MSP.admin'), SignedBy('Org3MSP.member'))

identities: [
    MSPPrincipal{ROLE, {msp: "Org1MSP", role: ADMIN}},   // index 0
    MSPPrincipal{ROLE, {msp: "Org2MSP", role: ADMIN}},   // index 1
    MSPPrincipal{ROLE, {msp: "Org3MSP", role: MEMBER}},  // index 2
]
rule: NOutOf(2, [
    NOutOf(1, [SignedBy(0), SignedBy(1)]),   // OR: either admin
    SignedBy(2),                              // AND: plus Org3 member
])
```

### Channel Policies for Lifecycle Operations

Namespace creation and updates (transactions in the `_meta` namespace) are not governed by per-namespace
policies. Instead, they use the channel-level **LifecycleEndorsement** policy at path
`/Channel/Application/LifecycleEndorsement`. This is an ImplicitMeta policy that aggregates
org-level endorsement policies — typically `MAJORITY Endorsement`, meaning a majority of organizations
must endorse lifecycle changes.

## 5. Cached Identities

In standard Fabric, every endorsement carries the full X.509 certificate of the signer (~1KB in PEM format).
Cached identities allow endorsers to send a short identity ID instead, reducing bandwidth from ~1KB to ~80 bytes
per endorsement.

### Wire Format

The `Identity` message in each `EndorsementWithIdentity` uses one of two formats:

| Field                    | Certificate Format                       | Cached ID Format                           |
|--------------------------|------------------------------------------|--------------------------------------------|
| `msp_id`                 | `"Org1MSP"`                              | `"Org1MSP"`                                |
| `creator`                | `certificate`: full X.509 cert (~1KB)    | `certificate_id`: hex SHA256 string (64 chars) |
| Bandwidth per endorsement| ~1,000 bytes                             | ~80 bytes                                  |

**Endorsement with cached ID:**

```go
// Without caching — full certificate (~1KB)
eid.Identity = msppb.NewIdentity("Org1MSP", certPEMBytes)

// With caching — identity ID (64 bytes)
eid.Identity = msppb.NewIdentityWithIDOfCert("Org1MSP", certIDHex)
```

The identity ID is computed as `hex(SHA256(cert.Raw))` where `cert.Raw` is the DER-encoded certificate
bytes (not PEM).

### Provisioning

Cached identities are provisioned through the channel configuration. Each organization's MSP folder
includes a `knowncerts/` directory containing the certificates of its known endorsers. These
certificates are included in the `FabricMSPConfig.known_certs` field:

```protobuf
message FabricMSPConfig {
    // ... other fields ...
    repeated bytes known_certs = 12;  // Pre-registered endorser certificates
}
```

Add endorser certificates to the `knowncerts/` directory in the organization's MSP folder before
generating the channel configuration. The endorser computes its own certificate ID using the same
`hex(SHA256(cert.Raw))` formula to include in endorsements.

When a new config block arrives (e.g., adding a new endorser certificate to `known_certs`), the MSP
is re-initialized with the updated identity mappings.

**Recommendation:** Cached identities are recommended for all production deployments where the same
signers endorse transactions repeatedly. The bandwidth savings compound with TPS and endorser count.

## 6. Policy Lifecycle

### Creating Namespace Policies

Namespace policies are defined through transactions in the **`_meta` namespace**.
A meta namespace transaction contains key-value pairs where:

- **Key**: The namespace ID (e.g., `"my_namespace"`)
- **Value**: Serialized `NamespacePolicy` protobuf (either a ThresholdRule or MspRule)

Meta namespace transactions are governed by the channel-level LifecycleEndorsement policy (see
[§4, Channel Policies](#channel-policies-for-lifecycle-operations)).

### Namespace ID Constraints

Namespace IDs must satisfy the following constraints:

| Constraint             | Rule                                                          |
|------------------------|---------------------------------------------------------------|
| **Length**             | 1 to 60 characters                                           |
| **Allowed characters** | Lowercase letters (`a-z`), digits (`0-9`), underscores (`_`) |
| **Pattern**            | `^[a-z0-9_]+$`                                               |
| **Reserved names**     | `_meta` and `_config` cannot be used as namespace IDs         |

The 60-character limit derives from PostgreSQL's identifier length limit (`NAMEDATALEN - 1 = 63`),
minus the 3-character `ns_` prefix used for namespace tables in the state database.
