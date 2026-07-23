---
name: development
description: >-
  Conventions for writing NEW Go code in the Fabric-X Committer repo: code
  organization/ordering, concurrency (errgroup + context-aware channels), Go
  1.26 idioms, error handling, logging, and service/config/metrics structure.
  Use this skill BEFORE writing or modifying any Go source in this repository —
  adding a service, component, function, gRPC handler, or config field, or
  refactoring existing code — so the new code matches established patterns and
  passes `make lint`. For test-only work use the `tests` skill; for reviewing a
  PR use `pr-review`.
---

# Developing in Fabric-X Committer

This skill tells you how to write new Go code that looks like it was already
here. Follow it whenever you add or change production Go source.

**Scope split** — write tests using the `tests` skill (it owns table-tests,
`t.Parallel`, metrics helpers). Review PRs with the `pr-review` skill. This skill
covers *authoring* production code.

**Authoritative sources in the repo** (read them when a topic needs depth):
- `guidelines.md` — the simplicity philosophy and error-handling policy.
- `docs/core-concurrency-pattern.md` — the errgroup + context-aware channel doctrine.
- `.golangci.yml` — every linter that gates your PR (see the cheat sheet at the end).
- `references/service-construction.md` (in this skill) — the full blueprint for a new
  service/component: struct, lifecycle, gRPC handlers, config pipeline, metrics, wiring.

## The prime directive: simplicity over cleverness

`guidelines.md` is emphatic and the whole codebase reflects it. Before adding any
abstraction, stop:

- **No premature interfaces.** Return and pass concrete `*T`. Interfaces are only for
  genuinely pluggable seams (e.g. `serve.Service`, DB adapters). The `ireturn` linter
  blocks most interface returns.
- **Generics only for reusable `utils/`-style plumbing** (`channel.ReaderWriter[T]`,
  `SyncMap[K,V]`, `retry.ExecuteWithResult[T]`). Core service logic uses concrete types.
- **No functions-as-arguments / callbacks.** If logic must be pluggable, define an
  interface and pass a concrete implementation (Strategy pattern). Closures are fine
  *inside* one function; don't pass them across API boundaries.
- **Explicit and a few lines longer beats compact and clever.** Guard clauses, early
  returns, intermediate variables. Max ~3 levels of nesting; `gocognit` caps complexity
  at 15.

## Step 0 — reuse before you write

Grep `utils/` before implementing anything infrastructural. Re-inventing these is the
most common review rejection:

| Need | Use | Package |
|------|-----|---------|
| Spawn goroutines & wait | `errgroup.WithContext` | `golang.org/x/sync/errgroup` |
| Move data over a channel safely | `channel.NewReader/NewWriter/Make` | `utils/channel` |
| Signal / wait for readiness | `channel.NewReady` | `utils/channel` |
| Bounded in-flight work (semaphore) | `Slots` | `utils/slots.go` |
| Typed concurrent map | `SyncMap[K,V]` | `utils/sync_map.go` |
| Create/originate/wrap errors | `errors.New/Newf/Wrap/Wrapf` | `github.com/cockroachdb/errors` |
| Convert error → gRPC status | `grpcerror.Wrap*` | `utils/grpcerror` |
| Dial gRPC peers | `connection.NewSingleConnection` / `NewLoadBalancedConnection` / `NewConnectionPerEndpoint` | `utils/connection` |
| Retry / keep a loop alive | `retry.Sustain`, `retry.WaitForCondition` | `utils/retry` |
| Run a service + gRPC/HTTP servers | `serve.StartAndServe` | `utils/serve` |
| Prometheus metrics | `monitoring.Provider` + `promutil.*` | `utils/monitoring` |
| Package logger | `flogging.MustGetLogger` | `fabric-lib-go/common/flogging` |

**Test fixtures** — test code has its own reuse discipline: don't hand-roll crypto, TLS, or
block fixtures. Full picture is in the `tests` skill; the key helpers:

| Need | Use | Package |
|------|-----|---------|
| In-process service / DB / mock harnesses | `test.RunServiceForTest`, `testdb.PrepareTestEnv`, `mock.*` | `utils/test`, `utils/testdb`, `mock/` (this repo) |
| Proto assertions | `test.RequireProtoEqual` / `RequireProtoElementsMatch` | `utils/test` (this repo) |
| MSP / identity / Fabric config-block fixtures | `testcrypto.CreateOrExtendConfigBlockWithCrypto`, `ConfigBlock`, `PrepareBlockHeaderAndMetadata`, `GetPeersIdentities` / `GetConsenterIdentities` / `GetSigningIdentities` / `GetPeersMspDirs` | `github.com/hyperledger/fabric-x-common/utils/testcrypto` |
| Test TLS CAs & cert/key pairs | `tlsgen.NewCA()`, `CA`, `CertKeyPair` | `github.com/hyperledger/fabric-x-common/common/crypto/tlsgen` |
| Generate crypto material | `cryptogen` | `github.com/hyperledger/fabric-x-common/tools/cryptogen` |

## Code organization

This is where new code most often diverges from the repo. Match these exactly.

### Caller before callee (top-down reading order)

Place a function **above** the functions it calls, so a reader meets each function
before its dependencies. This is the single most consistent rule in the codebase.

Model file: `service/vc/committer.go` reads `newCommitter` → `run` → `commit` →
`commitTransactions` → its helpers, each defined below its caller.

```go
// service/vc/committer.go:50 — run() sits directly above the commit() it calls
func (c *transactionCommitter) run(ctx context.Context, db *database, numWorkers int) error {
	g, eCtx := errgroup.WithContext(ctx)
	for range numWorkers {
		g.Go(func() error { return c.commit(eCtx, db) }) // callee defined next
	}
	return g.Wait()
}

func (c *transactionCommitter) commit(ctx context.Context, db *database) error { /* ... */ }
```

### Group methods by struct

Keep all methods of one type contiguous; the constructor `newXxx` leads the block, then
its methods. Don't interleave two types' methods by chance.

**Exception:** when two types collaborate tightly in one file, top-down call order
(above) wins over strict grouping — order by the processing flow
(`service/sidecar/notify.go` does this deliberately).

### Don't fragment into tiny single-use helpers — but do manage complexity

Default to one cohesive function; don't extract a helper just to name a step that runs from
a single place. Extract when the logic is **reused** or names a genuinely distinct step.

The exception is complexity: `gocognit` caps a function at 15. When a function grows past
that, **prefer breaking it into helpers to bring the complexity down** over leaving an
unreadable monolith — extracting for complexity's sake is fine even if the helper is called
once. Reach for `//nolint:gocognit // <reason>` only when the logic is genuinely cohesive
and does not split cleanly (e.g. the ~130-line `prepare` at `service/vc/preparer.go:133`).
In test code, `gocognit` is sometimes simply suppressed with a scoped `//nolint`.

For logic shared *within* one function, use a local closure
(`service/vc/validator_committer_service.go:317` `sendLargeBatch`), not a method.

### File layout template

Every `.go` file follows this order (model: `service/coordinator/coordinator.go`):

1. Apache-2.0 header — the `/* ... */` block comment (the `goheader` linter requires it):
   ```go
   /*
   Copyright IBM Corp. All Rights Reserved.

   SPDX-License-Identifier: Apache-2.0
   */
   ```
2. `package` clause.
3. `import (...)` — three blank-line-separated groups: stdlib, third-party, then internal
   `github.com/hyperledger/fabric-x-committer/...` (`goimports` local-prefixes enforces this).
4. `var logger = flogging.MustGetLogger("<name>")` in the package's primary file.
5. `const (...)` block (defaults, SQL templates).
6. Type declarations, usually a single grouped `type ( ... )` block, main struct first with a doc comment.
7. `var (...)` of sentinel errors named `ErrXxx`, each with a doc comment.
8. Constructor `NewXxx`/`newXxx`.
9. Exported lifecycle methods (`Run`, `WaitForReady`, `RegisterService`), then internal
   methods in caller-before-callee order.
10. Free helper functions at the bottom.

### Split a package into role-named files

`config.go` (Config + `Default*` consts), `metrics.go` (`perfMetrics` + `newXxxMetrics`),
`<service>.go` (the server, constructor, lifecycle, gRPC handlers), then one file per
pipeline component/manager (`preparer.go`, `validator.go`, `committer.go`, `database.go`).
Keep files moderate (~200–750 lines); give a large concern its own file. Tests are
co-located as `<file>_test.go`; shared exported test helpers go in `test_exports.go`.

## Concurrency

Read `docs/core-concurrency-pattern.md` once. The rules:

### errgroup is the default for spawning + waiting

Use `errgroup.WithContext(ctx)`, spawn with `g.Go(func() error {...})`, block on
`g.Wait()`. **Do not** hand-roll `sync.WaitGroup`, and **do not** use channels to signal
completion — that is errgroup's job. Bound parallelism with a fixed worker count in a
`for range n` loop (the codebase does not use `errgroup.SetLimit`).

```go
g, eCtx := errgroup.WithContext(ctx)
for range numWorkers {
	g.Go(func() error { return c.commit(eCtx, db) })
}
return g.Wait()
```

- Pass the group context (`eCtx`/`gCtx`) into goroutines, **not** the parent `ctx`.
- Guard long-running loops with `for ctx.Err() == nil` or `select { case <-ctx.Done(): }`.
- When any goroutine's exit must tear down the rest, wrap with `ctx, cancel :=
  context.WithCancel(ctx); defer cancel()` and `defer cancel()` inside each `g.Go`.
- A goroutine with nothing to return still uses errgroup and `return nil` (or `ctx.Err()`).
- Wrap the result at the boundary: `utils.ProcessErr(g.Wait(), "...")` or `errors.Wrap(g.Wait(), "...")`.

**`sync.WaitGroup` is a narrow exception:** use it only when you must wait for *every*
goroutine even after one fails (errgroup cancels siblings on first error). If so, prefer
the Go 1.25 method form `wg.Go(func(){...})` and collect errors with `errors.Join` — see
`utils/deliverorderer/orderer.go:448`.

### Channels carry data, never "done" — and always through the wrapper

Raw channels are for data flow between pipeline stages. Never `select` on a raw channel
plus `ctx.Done()` by hand; wrap it in `channel.NewReader`/`NewWriter`/`Make` so the
operation releases immediately on cancellation and the system never hangs on shutdown
(76+ call sites; rationale in `utils/channel/reader_writer.go:14`).

```go
incoming := channel.NewReader(ctx, c.incomingValidatedTransactions)
outgoing := channel.NewWriter(ctx, c.outgoingTransactionsStatus)
for {
	vTx, ok := incoming.Read()
	if !ok { return nil } // ctx done or channel closed
	// ...
	outgoing.Write(txsStatus)
}
```

Do not `for range aChannel` in production code — use the wrapper's `Read()` loop instead.

### Readiness and shutdown

Signal readiness with `channel.NewReady()` (`SignalReady` / `WaitForReady(ctx)` /
`Reset`), not a bare channel or bool. Make `Stop`/`Close` idempotent with `sync.Once`.
Use `context.AfterFunc(ctx, fn)` for cancel-driven teardown (the standard primitive here).
Reach for `atomic` for counters/flags/pointers, `Mutex`/`RWMutex` for compound state, and
`TryLock` to reject a second concurrent stream (`grpcerror.WrapFailedPrecondition`).

## Modern Go idioms (1.26)

The codebase is aggressively modern. **Prefer** in new code:

- `slices.*` — `Contains`, `Index`, `Clone`, `SortFunc`, `Sorted`, `Backward`, `Delete`,
  and `Collect` to consume iterators.
- `maps.*` consumed via `slices.Collect(maps.Values(m))` / `slices.Sorted(maps.Keys(m))`;
  `maps.Clone`, `maps.Copy`.
- Built-in `min` / `max` (pervasive), e.g. `min(high, max(low, v))`.
- Range-over-integer: `for range n` / `for i := range n` (the `intrange` linter pushes this).
- `iter.Seq` / `iter.Seq2` for custom iterators (`utils/sync_map.go`).
- `errors.Join` — especially to combine a retry sentinel with a cause:
  `errors.Join(retry.ErrBackOff, err)`.
- `context.AfterFunc`, and `context.WithCancelCause` / `context.Cause` where a cancel
  reason matters.
- `any` — **never** `interface{}` (only generated `.pb.go` uses the latter).
- Generics for `utils/`-style reusable helpers; concrete types for service logic.

**Absent — don't assume idiomatic; introduce only with a clear reason:** `clear()`,
`sync.OnceFunc`/`OnceValue` (use plain `sync.Once`), `cmp.Or`, `context.WithoutCancel`,
`log/slog` (use `flogging`), and `slices.Sort`/`Equal`/`Compact`.

## Error handling

Uses `github.com/cockroachdb/errors` (the `depguard` linter bans `github.com/pkg/errors`).

**Decision rule:**
1. **Originate or first cross into our code** (a new condition, or an error from a DB
   driver / gRPC `Recv` / marshal) → `errors.New` / `errors.Newf` / `errors.Wrap` /
   `errors.Wrapf`. This captures the stack trace at the origin.
2. **Add context while propagating** an error that already carries a trace →
   `fmt.Errorf("context: %w", err)`. Always `%w`, never `%v`/`%s` (the `errorlint` linter
   enforces this). This adds a message without a redundant second trace.
3. **Return errors up the stack; do not log mid-stack.** Log once, at the boundary.

**Pitfall — double-wrapping our own errors.** When the error already came from our code
(any `service/`, `utils/`, or helper that already `errors.Wrap`-ped it), do NOT re-wrap
with `errors.Wrap/Wrapf` — that captures a second, redundant stack trace at the same
logical point. Use `fmt.Errorf("context: %w", err)` to add context while preserving the
original trace. Reserve `errors.Wrap/Wrapf` for the origin (external lib or first crossing
into our code). Example: `readStatusWithHeight` already wraps, so a caller adding the txID
uses `fmt.Errorf("failed to read status for tx %s: %w", txID, err)`, not `errors.Wrapf`.

```go
if len(query.TxIds) == 0 {
	return nil, grpcerror.WrapInvalidArgument(errors.New("query is empty"))
}
txIDsStatus, err := vc.db.readStatusWithHeight(ctx, txIDs)
if err != nil {
	logger.Errorf("%+v", err)                    // log full trace, once, at the boundary
	return nil, grpcerror.WrapInternalError(err) // convert to a gRPC status code
}
```

**At gRPC handlers**, convert every returned error through a `grpcerror.Wrap*` helper —
never leak a raw internal error (stack traces must not cross the wire). Available
(`utils/grpcerror/wrap.go`, all nil-safe): `WrapInternalError`, `WrapInvalidArgument`,
`WrapCancelled`, `WrapFailedPrecondition`, `WrapUnimplemented`, `WrapNotFound`,
`WrapResourceExhaustedOrCancelled(ctx, err)`, `WrapWithContext(err, ctx)`. Map sentinels
to codes with an `errors.Is` chain (`service/query/query_service.go:382`).

**Sentinel errors:** declare `var ErrXxx = errors.New("...")` with a doc comment in a
`var (...)` block; compare with `errors.Is`, never `==` (the `errname` linter enforces the
`ErrXxx` / lowercase `xxxError` naming). For a custom error type, implement `Unwrap()` so
`errors.Is`/`As` traverse it.

## Logging

One unexported package-level logger via Fabric's zap-based `flogging`; no `New()`, no
per-struct logger, no logger passed as an argument:

```go
var logger = flogging.MustGetLogger("coordinator")
```

Use the printf-style methods `Debugf` / `Infof` / `Warnf` / `Errorf` (structured
key-value fields are not used here). `Debugf` for per-tx / hot-path detail, `Infof` for
lifecycle, `Warnf` (sparingly) for recoverable conditions, `Errorf("%+v", err)` at
boundaries. No `Fatal`/`Panic` in services — return an error. Bracket IDs and numbers as
`[%s]` / `[%d]`.

## Building a service or component

For anything larger than a function — a new service, gRPC server, manager, or pipeline
stage — read `references/service-construction.md`. It covers the full blueprint with
citations: the `serve.Service` interface (`Run`/`WaitForReady`/`RegisterService`), the
struct + `NewXxxService` constructor (open I/O in `Run`, not the constructor), gRPC
handler style, the config pipeline (`mapstructure`/`validate` tags → viper defaults →
sample YAML → decoder hooks), Prometheus metrics via `monitoring.Provider`, connection
wiring, `serve.StartAndServe` bootstrap, and a step-by-step checklist.

Quick essentials:
- Constructors return concrete `*T` with **no error** for pure in-memory wiring; return
  `(*T, error)` only when the constructor does I/O (e.g. `newDatabase`).
- Pass wide dependency lists as a `*xxxConfig` struct, not many positional args (the
  `revive` `argument-limit` is 4).
- Config structs use `mapstructure:"kebab-case"` + `validate:"..."` tags — **no `yaml`
  tags**. Every field needs a `Default*` const and a `v.SetDefault(...)` in
  `cmd/config/viper.go`, plus an entry in the sample YAML under `cmd/config/samples/`.
- Adding a `loadgen` `workload.Profile` field also means updating
  `loadgen_shared.yaml.tmpl` and the env-override test.

## Before you finish

Before you consider the change done (see `CLAUDE.md` and the Makefile):

- `make lint` (or `make lint-go`) — must pass with no errors.
- **Do not run the full `make test` locally — it takes too long.** Run only the relevant
  unit and integration tests for the packages you touched (e.g.
  `go test ./service/vc/... -run TestXxx`), then rely on CI for the full suite and monitor
  it. Add tests per the `tests` skill.
- If you changed metrics, run `make generate-metrics-doc`.
- If you changed `.proto`, run `make proto`.
- **Audit docs & config for staleness.** After the above pass, dispatch a subagent to
  run the `doc-audit` skill (keeps this context clean). It auto-fixes the generated docs
  and reports any prose, agent-instruction, skill, config-sample, or `.tmpl` template
  your change made obsolete — apply the **Review** suggestions it returns.

**Enforced-linter cheat sheet** (from `.golangci.yml`) — write to these up front so lint
passes the first time:

- `goheader` — Apache-2.0 `/* */` header on every file.
- `gofumpt` + `goimports` — formatting and 3-group import ordering (run `make lint`, it fixes most).
- `lll` / `revive line-length-limit` — 120-column lines.
- `intrange` — use `for range n`. `ireturn` — don't return interfaces. `errname` — `ErrXxx`.
- `errorlint` — `fmt.Errorf` must use `%w`. `depguard` — no `github.com/pkg/errors`.
- `gocognit` ≤ 15, `maintidx`, `dupl` — keep functions simple and non-duplicated (or justify with a scoped `//nolint:<linter> // reason`).
- `revive argument-limit` 4, `function-result-limit` 3 — use a config/param struct or grouped returns beyond these.
- `godot` — doc comments end with a period (does not apply to log strings).
- `containedctx` / `fatcontext` — don't store a `context.Context` in a struct (rare, justified exceptions carry `//nolint:containedctx`).
- `prealloc`, `unparam`, `wastedassign`, `unconvert`, `forcetypeassert`, `nilerr` — the usual correctness/efficiency nits.

`//nolint` must be specific (`//nolint:gocognit // <reason>`) — the `nolintlint` linter
requires the linter name and the codebase expects a reason.
