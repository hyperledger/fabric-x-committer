<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# TLS Configuration for Committer Services
This guide explains how to configure Transport Layer Security (TLS) for Committer services. TLS ensures secure communication whether a service operates as a **server** (accepting incoming connections) or as a **client** (initiating outbound connections).

## Configuration Structure
Each service configuration may include a `tls` section. This section defines the TLS mode and the filesystem paths to the required certificate files.

### Parameters
> Note: In this document, “CA certificates” refer to **TLS root CA certificates** used to verify a peer’s **TLS certificate**.

| Field | Type | Description                                                                                   |
| :--- | :--- |:----------------------------------------------------------------------------------------------|
| **`mode`** | String | TLS operation mode: `none`, `tls`, `mtls`. Determines how credentials are built and enforced. |
| **`cert-path`** | String | Path to the server/client TLS certificate (public key).                                       |
| **`key-path`** | String | Path to the server/client TLS private key.                                                    |
| **`ca-cert-paths`** | List | Paths to **TLS root CA certificates** used to verify the peer’s TLS certificate.              |

### Modes explanation

| Mode       | Description                                                                                                                                                          |
|:-----------|:---------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **`none`** | Starts the server with insecure.NewCredentials().                                                                                                                    |
| **`tls`**  | Requires only the server certificate and its private key. The client has to verify them using the root CA that created them (client don't present any certificates). |
| **`mtls`** | Both sides of the connection must present valid certificates. The peer's certificate is verified against the trusted CAs defined in ca-cert-paths.                   |


## Configuration Examples

### Server Configuration
Use this configuration when the service is acting as a server (accepting incoming secure connections).

```yaml
tls:
  # starts the service with mutual tls.
  mode: mtls
  # loading certificates to present to the client.
  cert-path: /server-certs/public-key.pem
  key-path: /server-certs/private-key.pem
  # the root CA certificates that verify the client's credentials.
  ca-cert-paths:
    - /server-certs/ca-certificate.pem
```

### Client Configuration
Use this configuration when the service is acting as a client (initiating secure connections to another service).

```yaml
tls:
  # client starts with mutual tls.
  mode: mtls
  # loading the certificates to present to server.
  cert-path: /client-certs/public-key.pem
  key-path: /client-certs/private-key.pem
  # the root CA certificates that verify the server's credentials.
  ca-cert-paths:
    - /client-certs/ca-certificate.pem
```

## Orderer Client TLS

When a service connects to the ordering service, it acts as a **TLS client**. In this case, the configuration includes both:
1. Client-side TLS credentials under `orderer.tls`, and
2. Per-organization orderer connection details under `orderer.organizations`.

`orderer.organizations` maps **organization MSP IDs** to their orderer endpoints and the **TLS root CAs** used to verify those orderers’ TLS certificates. This supports multi-organization deployments where each org can define its own orderers and trust roots.

| Field | Type | Description |
| :--- | :--- | :--- |
| **`orderer.organizations`** | Map | Maps organization MSP IDs to their orderer connection details (endpoints + TLS root CAs). |
| **`orderer.organizations.<mspid>.endpoints`** | List | Orderer endpoints for the organization (e.g., `id=0,broadcast,deliver,orderer:7050`). |
| **`orderer.organizations.<mspid>.ca-cert-paths`** | List | Paths to **TLS root CA certificates** used to verify that org’s orderers’ TLS certificates. |
| **`orderer.tls.mode`** | String | TLS operation mode for the client connection to orderers: `none`, `tls`, `mtls`. |
| **`orderer.tls.cert-path`** | String | Path to the **client TLS certificate** (public key). Required for `mtls`. |
| **`orderer.tls.key-path`** | String | Path to the **client TLS private key**. Required for `mtls`. |
| **`orderer.tls.common-ca-cert-paths`** | List | Paths to shared **TLS root CA certificates** used to verify orderer TLS certificates across organizations. |

### Orderer client configuration example

```yaml
orderer:
  channel-id: mychannel

  # organizations maps organization MSP IDs to their orderer connection details.
  # Each org can define its own orderer endpoints and TLS root CAs used to verify orderer TLS certificates.
  organizations:
    org0:
      endpoints:
        - id=0,broadcast,deliver,orderer:7050
      # TLS root CA certificates used to verify the orderer's TLS certificate.
      ca-cert-paths:
        - /client-certs/ca-certificate.pem

  # Client-side TLS credentials used when connecting to orderers.
  tls:
    # TLS mode for the client connection: none, tls, mtls.
    mode: mtls

    # Client certificate/key (required for mtls).
    cert-path: /client-certs/public-key.pem
    key-path: /client-certs/private-key.pem

    # Additional TLS root CA certificates shared across orgs (used to verify orderer TLS certificates).
    common-ca-cert-paths:
      - /client-certs/ca-certificate.pem
```



## Database TLS

Some database connections only require **server authentication** (i.e., the client verifies the database server certificate). In this case, configure TLS with `mode: tls` and provide the **TLS root CA certificate** used to verify the database server’s TLS certificate.

| Field | Type | Description |
| :--- | :--- | :--- |
| **`tls.mode`** | String | TLS operation mode for the database connection: `none`, `tls`. |
| **`tls.ca-cert-path`** | String | Path to the **TLS root CA certificate** used to verify the database server’s TLS certificate. |

### Database TLS configuration example

```yaml
tls:
  # Database TLS configuration is built based on the selected mode: none, tls.
  mode: tls
  # TLS root CA certificate used to verify the database server's TLS certificate.
  ca-cert-path: /server-certs/ca-certificate.pem

```
