<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Load Generator Artifacts

The load generator can create all the cryptographic materials and configuration artifacts needed for an experiment.
These artifacts include:

- Certificate authorities (CA) and TLS certificates
- MSP (Membership Service Provider) configurations
- Orderer and peer organization credentials
- Genesis block and channel configuration

## Generating Artifacts

The path references in the [sample configuration files](../cmd/config/samples) point to artifacts generated using this tool.

To generate artifacts using the [loadgen sample configuration](../cmd/config/samples/loadgen.yaml):

```bash
loadgen make-artifacts -c ./cmd/config/samples/loadgen.yaml
```

This command outputs artifacts to `/root/artifacts` (as specified in the sample configuration).

## Output Structure

The generated directory structure contains all necessary cryptographic materials organized by organization type:

<!-- TREE MARKER START -->
```
.
├── arma.pb.bin
├── config-block.pb.bin
├── ordererOrganizations/orderer-org-0
│   ├── ca
│   │   ├── orderer-org-0-CA-cert.pem
│   │   └── priv_sk
│   ├── tlsca
│   │   ├── priv_sk
│   │   └── tlsorderer-org-0-CA-cert.pem
│   ├── msp
│   │   ├── admincerts/Admin@orderer-org-0.com-cert.pem
│   │   ├── cacerts/orderer-org-0-CA-cert.pem
│   │   ├── knowncerts
│   │   │   ├── Admin@orderer-org-0.com-cert.pem
│   │   │   ├── client@orderer-org-0.com-cert.pem
│   │   │   ├── consenter-org-0-cert.pem
│   │   │   └── orderer-0-org-0-cert.pem
│   │   └── tlscacerts/tlsorderer-org-0-CA-cert.pem
│   ├── orderers
│   │   ├── consenter-org-0
│   │   │   ├── tls
│   │   │   │   ├── ca.crt
│   │   │   │   ├── server.crt
│   │   │   │   └── server.key
│   │   │   └── msp
│   │   │       ├── admincerts/Admin@orderer-org-0.com-cert.pem
│   │   │       ├── cacerts/orderer-org-0-CA-cert.pem
│   │   │       ├── keystore/priv_sk
│   │   │       ├── signcerts/consenter-org-0-cert.pem
│   │   │       └── tlscacerts/tlsorderer-org-0-CA-cert.pem
│   │   └── orderer-0-org-0
│   │       ├── tls
│   │       │   ├── ca.crt
│   │       │   ├── server.crt
│   │       │   └── server.key
│   │       └── msp
│   │           ├── admincerts/Admin@orderer-org-0.com-cert.pem
│   │           ├── cacerts/orderer-org-0-CA-cert.pem
│   │           ├── keystore/priv_sk
│   │           ├── signcerts/orderer-0-org-0-cert.pem
│   │           └── tlscacerts/tlsorderer-org-0-CA-cert.pem
│   └── users
│       ├── Admin@orderer-org-0.com
│       │   ├── tls
│       │   │   ├── ca.crt
│       │   │   ├── client.crt
│       │   │   └── client.key
│       │   └── msp
│       │       ├── admincerts/Admin@orderer-org-0.com-cert.pem
│       │       ├── cacerts/orderer-org-0-CA-cert.pem
│       │       ├── keystore/priv_sk
│       │       ├── signcerts/Admin@orderer-org-0.com-cert.pem
│       │       └── tlscacerts/tlsorderer-org-0-CA-cert.pem
│       └── client@orderer-org-0.com
│           ├── tls
│           │   ├── ca.crt
│           │   ├── client.crt
│           │   └── client.key
│           └── msp
│               ├── admincerts/client@orderer-org-0.com-cert.pem
│               ├── cacerts/orderer-org-0-CA-cert.pem
│               ├── keystore/priv_sk
│               ├── signcerts/client@orderer-org-0.com-cert.pem
│               └── tlscacerts/tlsorderer-org-0-CA-cert.pem
└── peerOrganizations
    ├── peer-org-0
    │   ├── ca
    │   │   ├── peer-org-0-CA-cert.pem
    │   │   └── priv_sk
    │   ├── tlsca
    │   │   ├── priv_sk
    │   │   └── tlspeer-org-0-CA-cert.pem
    │   ├── msp
    │   │   ├── admincerts/Admin@peer-org-0.com-cert.pem
    │   │   ├── cacerts/peer-org-0-CA-cert.pem
    │   │   ├── knowncerts
    │   │   │   ├── Admin@peer-org-0.com-cert.pem
    │   │   │   ├── client@peer-org-0.com-cert.pem
    │   │   │   ├── coordinator-cert.pem
    │   │   │   ├── db-cert.pem
    │   │   │   ├── loadgen-cert.pem
    │   │   │   ├── query-cert.pem
    │   │   │   ├── sidecar-cert.pem
    │   │   │   ├── vc-cert.pem
    │   │   │   └── verifier-cert.pem
    │   │   └── tlscacerts/tlspeer-org-0-CA-cert.pem
    │   ├── peers
    │   │   ├── coordinator
    │   │   │   ├── tls
    │   │   │   │   ├── ca.crt
    │   │   │   │   ├── server.crt
    │   │   │   │   └── server.key
    │   │   │   └── msp
    │   │   │       ├── admincerts/Admin@peer-org-0.com-cert.pem
    │   │   │       ├── cacerts/peer-org-0-CA-cert.pem
    │   │   │       ├── keystore/priv_sk
    │   │   │       ├── signcerts/coordinator-cert.pem
    │   │   │       └── tlscacerts/tlspeer-org-0-CA-cert.pem
    │   │   ├── db
    │   │   │   ├── tls
    │   │   │   │   ├── ca.crt
    │   │   │   │   ├── server.crt
    │   │   │   │   └── server.key
    │   │   │   └── msp
    │   │   │       ├── admincerts/Admin@peer-org-0.com-cert.pem
    │   │   │       ├── cacerts/peer-org-0-CA-cert.pem
    │   │   │       ├── keystore/priv_sk
    │   │   │       ├── signcerts/db-cert.pem
    │   │   │       └── tlscacerts/tlspeer-org-0-CA-cert.pem
    │   │   ├── loadgen
    │   │   │   ├── tls
    │   │   │   │   ├── ca.crt
    │   │   │   │   ├── server.crt
    │   │   │   │   └── server.key
    │   │   │   └── msp
    │   │   │       ├── admincerts/Admin@peer-org-0.com-cert.pem
    │   │   │       ├── cacerts/peer-org-0-CA-cert.pem
    │   │   │       ├── keystore/priv_sk
    │   │   │       ├── signcerts/loadgen-cert.pem
    │   │   │       └── tlscacerts/tlspeer-org-0-CA-cert.pem
    │   │   ├── query
    │   │   │   ├── tls
    │   │   │   │   ├── ca.crt
    │   │   │   │   ├── server.crt
    │   │   │   │   └── server.key
    │   │   │   └── msp
    │   │   │       ├── admincerts/Admin@peer-org-0.com-cert.pem
    │   │   │       ├── cacerts/peer-org-0-CA-cert.pem
    │   │   │       ├── keystore/priv_sk
    │   │   │       ├── signcerts/query-cert.pem
    │   │   │       └── tlscacerts/tlspeer-org-0-CA-cert.pem
    │   │   ├── sidecar
    │   │   │   ├── tls
    │   │   │   │   ├── ca.crt
    │   │   │   │   ├── server.crt
    │   │   │   │   └── server.key
    │   │   │   └── msp
    │   │   │       ├── admincerts/Admin@peer-org-0.com-cert.pem
    │   │   │       ├── cacerts/peer-org-0-CA-cert.pem
    │   │   │       ├── keystore/priv_sk
    │   │   │       ├── signcerts/sidecar-cert.pem
    │   │   │       └── tlscacerts/tlspeer-org-0-CA-cert.pem
    │   │   ├── vc
    │   │   │   ├── tls
    │   │   │   │   ├── ca.crt
    │   │   │   │   ├── server.crt
    │   │   │   │   └── server.key
    │   │   │   └── msp
    │   │   │       ├── admincerts/Admin@peer-org-0.com-cert.pem
    │   │   │       ├── cacerts/peer-org-0-CA-cert.pem
    │   │   │       ├── keystore/priv_sk
    │   │   │       ├── signcerts/vc-cert.pem
    │   │   │       └── tlscacerts/tlspeer-org-0-CA-cert.pem
    │   │   └── verifier
    │   │       ├── tls
    │   │       │   ├── ca.crt
    │   │       │   ├── server.crt
    │   │       │   └── server.key
    │   │       └── msp
    │   │           ├── admincerts/Admin@peer-org-0.com-cert.pem
    │   │           ├── cacerts/peer-org-0-CA-cert.pem
    │   │           ├── keystore/priv_sk
    │   │           ├── signcerts/verifier-cert.pem
    │   │           └── tlscacerts/tlspeer-org-0-CA-cert.pem
    │   └── users
    │       ├── Admin@peer-org-0.com
    │       │   ├── tls
    │       │   │   ├── ca.crt
    │       │   │   ├── client.crt
    │       │   │   └── client.key
    │       │   └── msp
    │       │       ├── admincerts/Admin@peer-org-0.com-cert.pem
    │       │       ├── cacerts/peer-org-0-CA-cert.pem
    │       │       ├── keystore/priv_sk
    │       │       ├── signcerts/Admin@peer-org-0.com-cert.pem
    │       │       └── tlscacerts/tlspeer-org-0-CA-cert.pem
    │       └── client@peer-org-0.com
    │           ├── tls
    │           │   ├── ca.crt
    │           │   ├── client.crt
    │           │   └── client.key
    │           └── msp
    │               ├── admincerts/client@peer-org-0.com-cert.pem
    │               ├── cacerts/peer-org-0-CA-cert.pem
    │               ├── keystore/priv_sk
    │               ├── signcerts/client@peer-org-0.com-cert.pem
    │               └── tlscacerts/tlspeer-org-0-CA-cert.pem
    └── peer-org-1
        ├── ca
        │   ├── peer-org-1-CA-cert.pem
        │   └── priv_sk
        ├── tlsca
        │   ├── priv_sk
        │   └── tlspeer-org-1-CA-cert.pem
        ├── msp
        │   ├── admincerts/Admin@peer-org-1.com-cert.pem
        │   ├── cacerts/peer-org-1-CA-cert.pem
        │   ├── knowncerts
        │   │   ├── Admin@peer-org-1.com-cert.pem
        │   │   ├── client@peer-org-1.com-cert.pem
        │   │   ├── coordinator-cert.pem
        │   │   ├── db-cert.pem
        │   │   ├── loadgen-cert.pem
        │   │   ├── query-cert.pem
        │   │   ├── sidecar-cert.pem
        │   │   ├── vc-cert.pem
        │   │   └── verifier-cert.pem
        │   └── tlscacerts/tlspeer-org-1-CA-cert.pem
        ├── peers
        │   ├── coordinator
        │   │   ├── tls
        │   │   │   ├── ca.crt
        │   │   │   ├── server.crt
        │   │   │   └── server.key
        │   │   └── msp
        │   │       ├── admincerts/Admin@peer-org-1.com-cert.pem
        │   │       ├── cacerts/peer-org-1-CA-cert.pem
        │   │       ├── keystore/priv_sk
        │   │       ├── signcerts/coordinator-cert.pem
        │   │       └── tlscacerts/tlspeer-org-1-CA-cert.pem
        │   ├── db
        │   │   ├── tls
        │   │   │   ├── ca.crt
        │   │   │   ├── server.crt
        │   │   │   └── server.key
        │   │   └── msp
        │   │       ├── admincerts/Admin@peer-org-1.com-cert.pem
        │   │       ├── cacerts/peer-org-1-CA-cert.pem
        │   │       ├── keystore/priv_sk
        │   │       ├── signcerts/db-cert.pem
        │   │       └── tlscacerts/tlspeer-org-1-CA-cert.pem
        │   ├── loadgen
        │   │   ├── tls
        │   │   │   ├── ca.crt
        │   │   │   ├── server.crt
        │   │   │   └── server.key
        │   │   └── msp
        │   │       ├── admincerts/Admin@peer-org-1.com-cert.pem
        │   │       ├── cacerts/peer-org-1-CA-cert.pem
        │   │       ├── keystore/priv_sk
        │   │       ├── signcerts/loadgen-cert.pem
        │   │       └── tlscacerts/tlspeer-org-1-CA-cert.pem
        │   ├── query
        │   │   ├── tls
        │   │   │   ├── ca.crt
        │   │   │   ├── server.crt
        │   │   │   └── server.key
        │   │   └── msp
        │   │       ├── admincerts/Admin@peer-org-1.com-cert.pem
        │   │       ├── cacerts/peer-org-1-CA-cert.pem
        │   │       ├── keystore/priv_sk
        │   │       ├── signcerts/query-cert.pem
        │   │       └── tlscacerts/tlspeer-org-1-CA-cert.pem
        │   ├── sidecar
        │   │   ├── tls
        │   │   │   ├── ca.crt
        │   │   │   ├── server.crt
        │   │   │   └── server.key
        │   │   └── msp
        │   │       ├── admincerts/Admin@peer-org-1.com-cert.pem
        │   │       ├── cacerts/peer-org-1-CA-cert.pem
        │   │       ├── keystore/priv_sk
        │   │       ├── signcerts/sidecar-cert.pem
        │   │       └── tlscacerts/tlspeer-org-1-CA-cert.pem
        │   ├── vc
        │   │   ├── tls
        │   │   │   ├── ca.crt
        │   │   │   ├── server.crt
        │   │   │   └── server.key
        │   │   └── msp
        │   │       ├── admincerts/Admin@peer-org-1.com-cert.pem
        │   │       ├── cacerts/peer-org-1-CA-cert.pem
        │   │       ├── keystore/priv_sk
        │   │       ├── signcerts/vc-cert.pem
        │   │       └── tlscacerts/tlspeer-org-1-CA-cert.pem
        │   └── verifier
        │       ├── tls
        │       │   ├── ca.crt
        │       │   ├── server.crt
        │       │   └── server.key
        │       └── msp
        │           ├── admincerts/Admin@peer-org-1.com-cert.pem
        │           ├── cacerts/peer-org-1-CA-cert.pem
        │           ├── keystore/priv_sk
        │           ├── signcerts/verifier-cert.pem
        │           └── tlscacerts/tlspeer-org-1-CA-cert.pem
        └── users
            ├── Admin@peer-org-1.com
            │   ├── tls
            │   │   ├── ca.crt
            │   │   ├── client.crt
            │   │   └── client.key
            │   └── msp
            │       ├── admincerts/Admin@peer-org-1.com-cert.pem
            │       ├── cacerts/peer-org-1-CA-cert.pem
            │       ├── keystore/priv_sk
            │       ├── signcerts/Admin@peer-org-1.com-cert.pem
            │       └── tlscacerts/tlspeer-org-1-CA-cert.pem
            └── client@peer-org-1.com
                ├── tls
                │   ├── ca.crt
                │   ├── client.crt
                │   └── client.key
                └── msp
                    ├── admincerts/client@peer-org-1.com-cert.pem
                    ├── cacerts/peer-org-1-CA-cert.pem
                    ├── keystore/priv_sk
                    ├── signcerts/client@peer-org-1.com-cert.pem
                    └── tlscacerts/tlspeer-org-1-CA-cert.pem
```
<!-- TREE MARKER END -->
