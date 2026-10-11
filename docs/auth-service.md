<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Authentication & Authorization Service (ACL)

The `auth` service is a central Authentication & Authorization Service (`AuthService`) that guards the
gRPC APIs exposed by Committer-X. It authenticates a client once and issues a short-lived,
certificate-bound opaque token; resource servers (Query, Sidecar) then authorize each RPC by
delegating to the `AuthService`. This decouples the one-time, expensive signature verification from
the many cheap per-call authorization checks, and works behind client-side load balancing because the
authoritative state lives in one place (the state database), not on a socket or an instance.

## Structure

The service is composed of focused collaborators, each with one responsibility:

- **`configProvider`** (`config_provider.go`) — *reads* the latest committed channel configuration from the
  `ns__config` namespace and exposes it as a `channelconfig.Bundle`, refreshing on an interval. The auth
  service never owns or mutates configuration, it only reads what the sidecar and coordinator commit.
- **`database`** (`database.go`) — persists the token-to-identity bindings in the `auth_tokens` table,
  keyed by each token's SHA-256 (with the client's MSP identity, certificate binding, scope, and expiry),
  and the unredeemed nonces in the `auth_nonces` table.
- **`Service`** (`auth_service.go`) — owns the above and exposes the gRPC handlers: `Authenticate`
  verifies a signed envelope (scope, freshness, certificate binding, MSP identity, signature) and issues a
  token, and `Authorize` answers authorization decisions, with `policy.go` resolving the resource policy.

## RPCs

- `Authenticate(signed_envelope, requested_scope) -> token, expires_at` — verifies the envelope
  (channel and empty-payload scope, timestamp freshness, TLS certificate binding, MSP identity
  resolution, signature), issues a random opaque token, and writes its hash and the token-to-identity binding
  to the store.
- `IssueNonce() -> nonce, expires_at` — issues the single-use challenge a client must sign into its
  envelope. It is the mandatory first step of authentication.
- `Authorize(token, resource, tls_cert_hash) -> token_expires_at` — resolves the token's record from the
  store, checks its expiry, certificate binding and optional resource scope, validates the bound identity,
  evaluates the resource's policy against the latest configuration, and returns the
  token's expiry. A resource server calls it for every unary RPC and, for a stream, whenever its cached
  decision lapses.

Every non-authorized outcome is a gRPC status error:
`Unauthenticated` (invalid/expired/unknown token or certificate mismatch), `PermissionDenied`
(scope or policy denial), or `Unavailable` (before the configuration is loaded, or AuthService
unreachable).

## Replay protection: the nonce challenge

Authentication is a two-step challenge-response. The client calls `IssueNonce`, receives 32 bytes of
server-chosen randomness, and places it in the `SignatureHeader.Nonce` of the envelope it then presents
to `Authenticate`. Because the envelope's signature covers the whole marshaled payload — and the
`SignatureHeader` is part of that payload — the nonce cannot be substituted without invalidating the
signature.

The nonce is **consumed** when redeemed: `Authenticate` deletes the row and requires that exactly one
row was removed, so a second presentation of the same envelope fails. That single `DELETE` is what
makes the guarantee atomic even when two redemptions race, and even across instances.

Nonces are held in the shared state database (`auth_nonces`), not in one instance's memory, because a
client behind a load balancer has no guarantee its `IssueNonce` and `Authenticate` calls reach the same
instance. Unredeemed nonces are swept on the same tick as expired tokens.

This is what makes a captured envelope worthless rather than merely short-lived: the freshness window
and the certificate binding remain as defence in depth, but replay is closed by the nonce itself.

## Token and certificate binding

The token is 256 random bits, base64url-encoded, and carries no claims: it is a key into the token store
and nothing more. It needs no signature because only the `AuthService` resolves tokens, and it always
does so through the store - a token that was never issued has no record, and 256 random bits cannot be
guessed. The store is keyed by the token's SHA-256, never the token itself, so reading the database
yields nothing a client could present. A signed, self-describing token (such as a JWT) would only earn its
place if resource servers verified tokens locally, without the round trip to `Authorize`.

The certificate binding lives in the record, as the SHA-256 of the client's TLS certificate, and an
authorization compares it against the certificate the resource server observed - so a leaked token is
useless without the matching private key.

Whether the token is certificate-bound follows entirely from the transport: mutual TLS makes the
client's certificate present on the connection, and the service binds to it. There is no separate
"mutual TLS" toggle - the server's TLS mode is the one source of truth. The envelope's claimed hash must
match the connection exactly, so a client's TLS mode must match the servers': a client configured for
mutual TLS against a server that does not request its certificate claims a hash the connection cannot
back, and `Authenticate` refuses it rather than silently issuing an unbound token.

**Mutual TLS is required, not optional.** The certificate binding is the token's proof-of-possession
property. Without mutual TLS the connection presents no client certificate, the token is not
certificate-bound, and it degrades to a plain bearer token that anyone who captures it can replay.
Deploy the `AuthService` and every ACL-protected resource server with `mode: mtls` end to end; the
non-mTLS path exists only for local development and tests and must not be used in production.

## Client helpers (`utils/acl`)

`IssueToken` fetches a nonce, signs it into an envelope (`BuildAuthEnvelope`) and exchanges it for a token;
`TLSCertHash` gives the certificate hash a mutual-TLS client binds its token to.

## Configuration

- **`AuthService`** (`cmd/config/samples/auth.yaml`): `token-ttl`, `envelope-freshness-window`,
  `nonce-ttl`, `config-refresh-interval`, `sweep-interval`, `challenge-rate-limit`, and the state
  `database`, whose `retry` profile governs every query. Use `mtls` for the server TLS mode so tokens are
  certificate-bound.

## Policy resolution

A resource is its gRPC full-method name (e.g. `/committerpb.QueryService/GetRows`). The policy is
resolved from the channel configuration's `ACLs` section first, then a hard-coded default map
(`policy.go`), which maps every exposed method to `/Channel/Application/Readers`. If neither defines a
policy, the request is denied.

## Operational notes

- **Replay resistance.** The single-use nonce is the primary guard (see *Replay protection* above).
  Behind it: an authentication envelope is scoped to this channel and must carry an empty payload, so an
  ordinary transaction - which shares the envelope's header type and channel but always carries a
  payload - cannot be replayed to `Authenticate` to issue a token in its signer's name, a necessary guard
  because any channel reader can observe committed transactions. The freshness window bounds how long an
  unredeemed envelope stays presentable, and under mutual TLS the certificate binding makes a captured
  envelope useless without the signer's private key.
- **Rate limiting the front door.** `IssueNonce` and `Authenticate` are reachable without a token, and
  each costs the service real work - a database row for a nonce, a signature verification and MSP
  resolution for an authentication - so they share a token-bucket limit
  (`challenge-rate-limit`, default 200/s, burst 50) and return
  `ResourceExhausted` above it. This is deliberately separate from the server-wide `rate-limit`:
  `Authorize` is called once per protected RPC by the resource servers, so its rate tracks the whole
  cluster's traffic and must not share a bucket with the two unauthenticated RPCs. The limit is global
  per instance, not per client, so it caps total load rather than isolating one caller; mTLS is what
  restricts who can reach the service at all.
- **Cost per protected RPC.** Every unary call, stream establishment and stream re-authorization is one
  `Authorize` round trip plus one primary-key read of the token store. There is no decision cache on
  either side; add one only once load testing shows this round trip matters, and note that a cache
  would also delay revocation (below).
- **`Authorize` is callable directly.** It is not restricted to resource servers, so a caller holding a
  token can learn whether it is valid. That reveals nothing a token holder could not learn by using it,
  and a token cannot be guessed; mTLS is what restricts who reaches the service.
- **No revocation.** A token is valid until its `expires_at`, and `token-ttl` is the only lever on how long a
  compromised token or a withdrawn authority remains usable. No RPC deletes a record, though with every
  lookup reading the store, a delete would take effect on all instances at once - so revocation is cheap
  to add later (it would stop being so if a token cache is introduced). `Authorize` checks the record's
  `expires_at` on every call rather than relying on the sweep.
- **Namespace scope is deferred.** Restricting a token to particular namespaces is future work, not part
  of this iteration. Only `GetRows` and `StreamBlocks` name the namespaces they touch; block
  query and block delivery return whole blocks spanning every namespace, and a block cannot be filtered
  to a subset without breaking the verification clients perform on it. A scope enforceable on two
  resources and meaning "denied entirely" on the rest is a poor primitive, and the obvious
  implementation fails open, since a request naming no namespaces means *every* namespace. See the RFC's
  "Future work: namespace scoping".
- **Bootstrap.** Until the first configuration block is committed and observed, the `AuthService`
  returns `Unavailable`; protected APIs reject calls until enforcement becomes active.
