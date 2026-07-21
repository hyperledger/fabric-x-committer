# Building a service or component

Read this when adding a new service, gRPC server, manager, or pipeline component.
All patterns are verified across `coordinator`, `vc`, `verifier`, and `sidecar`.
Cite the model files rather than guessing.

## Table of contents

1. [The `serve.Service` interface](#1-the-serveservice-interface)
2. [Service struct + constructor](#2-service-struct--constructor)
3. [Lifecycle: `Run` / `WaitForReady` / `RegisterService`](#3-lifecycle)
4. [gRPC handler style](#4-grpc-handler-style)
5. [Config structs and the loading pipeline](#5-config-structs-and-the-loading-pipeline)
6. [Metrics](#6-metrics)
7. [Dependency wiring](#7-dependency-wiring)
8. [Server bootstrap](#8-server-bootstrap)
9. [New-service checklist](#9-new-service-checklist)

---

## 1. The `serve.Service` interface

A full-lifecycle service implements three methods (`utils/serve/start_serve.go:31`):

```go
type Service interface {
	Registerer                              // RegisterService(Servers)
	Run(ctx context.Context) error          // run until ctx is done
	WaitForReady(ctx context.Context) bool  // false if ctx ends before ready
}
```

You never create a `grpc.Server` yourself — you implement this interface and hand the
service to `serve.StartAndServe` (see [§8](#8-server-bootstrap)).

## 2. Service struct + constructor

The struct embeds the generated `servicepb.Unimplemented<X>Server`, and holds `*Config`,
a metrics struct, a `*health.Server`, internal channels/queues, and a `*channel.Ready`.
Name the constructor `New<Xxx>Service(config)` (the verifier uses plain `New`).

```go
// service/verifier/verifier_server.go:28
type Server struct {
	servicepb.UnimplementedVerifierServer
	config      *Config
	metrics     *metrics
	healthcheck *health.Server
}

func New(config *Config) *Server {
	return &Server{
		config:      config,
		metrics:     newMonitoring(),
		healthcheck: serve.DefaultHealthCheckService(),
	}
}
```

Rules:
- The constructor does **pure in-memory wiring** (channels, metrics, healthcheck). It does
  **not** open DB or gRPC connections — those happen in `Run`.
- Return concrete `*T` with **no error** for in-memory wiring. Return `(*T, error)` only
  when construction does I/O (e.g. `newDatabase(ctx, cfg, metrics) (*database, error)`,
  `service/vc/database.go:76`, or `sidecar.New` which parses orderer params eagerly).
- Unexported components use lowercase `newXxx` and log `logger.Info("Initializing
  new<Xxx>")` as the first line.
- The body returns a keyed struct literal, one `field: value,` per line.

Model files: `service/coordinator/coordinator.go:110`,
`service/vc/validator_committer_service.go:72`, `service/sidecar/sidecar.go:85`.

## 3. Lifecycle

### `Run(ctx) error`

Opens resources (with `defer close`), signals ready, fans out workers under an errgroup,
and blocks on `g.Wait()`:

```go
// service/vc/validator_committer_service.go:107 (condensed)
func (vc *ValidatorCommitterService) Run(ctx context.Context) error {
	db, err := newDatabase(ctx, vc.config.Database, vc.metrics)
	if err != nil {
		return err
	}
	defer db.close()
	vc.db = db

	vc.ready.SignalReady()
	defer vc.ready.Reset()

	g, eCtx := errgroup.WithContext(ctx)
	g.Go(func() error { vc.monitorQueues(eCtx); return nil })
	// ... more g.Go(...) worker pools ...
	if err := g.Wait(); err != nil {
		return err
	}
	return nil
}
```

- There is **no separate `Close`/`Stop` on the service** — cleanup is `defer`-based inside
  `Run`; server-side shutdown is owned by `serve.Servers`.
- A service with nothing to start just blocks on ctx and returns
  (`service/verifier/verifier_server.go:52`).
- A service that must tear down peers on any goroutine's exit wraps with
  `ctx, cancel := context.WithCancel(ctx); defer cancel()`
  (`service/coordinator/coordinator.go:184`, `service/sidecar/sidecar.go:141`).
- Resilient background loops use `retry.Sustain(ctx, profile, op)`
  (`service/sidecar/sidecar.go:160`).

### `WaitForReady(ctx) bool`

Delegate to a `*channel.Ready`:

```go
func (vc *ValidatorCommitterService) WaitForReady(ctx context.Context) bool {
	return vc.ready.WaitForReady(ctx)
}
```

A composite service gates its own readiness on sub-managers being ready first
(`service/coordinator/coordinator.go:218`).

### `RegisterService(serve.Servers)`

Register the gRPC service, the health server, and the monitoring HTTP endpoint. Identical
shape across all services — no nil checks needed (`NewServers` always builds both servers):

```go
// service/coordinator/coordinator.go:242
func (c *Service) RegisterService(s serve.Servers) {
	servicepb.RegisterCoordinatorServer(s.GRPC, c)
	healthgrpc.RegisterHealthServer(s.GRPC, c.healthcheck)
	monitoring.RegisterMonitoringServer(s.HTTP, c.metrics.Provider)
}
```

## 4. gRPC handler style

At the boundary: validate input, log full errors, convert every returned error through a
`grpcerror.Wrap*` helper. Never return a raw internal error.

```go
// service/vc/validator_committer_service.go:210
func (vc *ValidatorCommitterService) GetTransactionsStatus(
	ctx context.Context, query *committerpb.TxIDsBatch,
) (*committerpb.TxStatusBatch, error) {
	if len(query.TxIds) == 0 {
		return nil, grpcerror.WrapInvalidArgument(errors.New("query is empty"))
	}
	txIDsStatus, err := vc.db.readStatusWithHeight(ctx, txIDs)
	if err != nil {
		logger.Errorf("%+v", err)
		return nil, grpcerror.WrapInternalError(err)
	}
	return &committerpb.TxStatusBatch{Status: txIDsStatus}, nil
}
```

**Streaming handlers:** guard a single active stream with `TryLock` (or an atomic CAS)
returning `WrapFailedPrecondition`, then run send/receive loops in an errgroup bound to
`stream.Context()`, wrapping `Recv`/`Send` errors with `errors.Wrap`.

```go
// service/coordinator/coordinator.go:335
if !c.streamActive.TryLock() {
	return grpcerror.WrapFailedPrecondition(ErrExistingStreamOrConflictingOp)
}
defer c.streamActive.Unlock()
```

Models: `service/vc/validator_committer_service.go:266` (stream + CAS guard),
`service/verifier/verifier_server.go:71` (per-stream executor).

## 5. Config structs and the loading pipeline

Per-service `config.go` defines `Config` with `mapstructure:"kebab-case"` +
`validate:"..."` tags. **No `yaml` tags.** Durations are plain `time.Duration` (decoded
from strings like `"5s"` by a decoder hook). Endpoints use `connection.Endpoint`; client
deps use `connection.MultiClientConfig` / `ClientConfig`. Nested optional sub-configs are
pointers tagged `validate:"required"`. Serving knobs (server/monitoring endpoints, TLS,
keepalive, rate-limit) live in `serve.Config`, **not** the service config.

```go
// service/coordinator/config.go:17
type Config struct {
	Verifier           connection.MultiClientConfig `mapstructure:"verifier"`
	ValidatorCommitter connection.MultiClientConfig `mapstructure:"validator-committer"`
	DependencyGraph    *DependencyGraphConfig       `mapstructure:"dependency-graph" validate:"required"`
	ChannelBufferSizePerGoroutine int          `mapstructure:"per-channel-buffer-size-per-goroutine" validate:"required,gt=0"`
	QueueMonitorSamplingTime      time.Duration `mapstructure:"queue-monitor-sampling-time" validate:"required,gt=0"`
}

const (
	DefaultServerPort             = 6001
	DefaultDatabaseMaxConnections = 20
)
```

The full pipeline (keep every stage in sync when adding a field):

```
Config struct (mapstructure) → viper default → sample YAML → env var → decoder hook → docs
```

- Register defaults in `cmd/config/viper.go` via `v.SetDefault("kebab.key", Default*)`;
  env vars use prefix `SC_<SERVICE>_` with `-`/`.`→`_`.
- `readYamlAndSetupLogging[T]` (`cmd/config/app_config.go:90`) reads YAML, applies the
  `SC_<SVC>_YAML` override, and unmarshals + `validate.Struct`s three structs: logging,
  `serve.Config`, and your `T`.
- Custom decode hooks (`time.Duration`, byte sizes, `Endpoint`, `serve.ServerConfig`) live
  in `cmd/config/config_decoder.go`.
- Add a sample under `cmd/config/samples/`; comment the **why/implications**, not just the
  what. For `loadgen` `workload.Profile` fields, also update `loadgen_shared.yaml.tmpl` and
  the env-override test.

## 6. Metrics

`metrics.go` defines a `perfMetrics` struct that **embeds `*monitoring.Provider`** and
holds typed prometheus fields. `newXxxMetrics()` calls `monitoring.NewProvider()` then
`p.NewCounter/NewGauge/NewHistogram(...)` (each auto-registers). Give every metric
`Namespace`/`Subsystem`/`Name`/`Help`. Update metrics at runtime only through `promutil`
helpers (`AddToCounter`, `SetGauge`, `Observe`), never raw prometheus calls.

```go
// service/vc/metrics.go:15
var buckets = []float64{.0001, .001, .002, .003, .004, .005, .01, .03, .05, .1, .3, .5, 1}

type perfMetrics struct {
	*monitoring.Provider
	transactionReceivedTotal      prometheus.Counter
	preparerInputQueueSize        prometheus.Gauge
	preparerTxBatchLatencySeconds prometheus.Histogram
}

func newVCServiceMetrics() *perfMetrics {
	p := monitoring.NewProvider()
	return &perfMetrics{
		Provider: p,
		transactionReceivedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: "vcservice", Subsystem: "grpc",
			Name: "received_transaction_total", Help: "Number of transactions received.",
		}),
	}
}
```

Reuse `monitoring.ThroughputMetrics` / `ConnectionMetrics` bundles where they fit
(`utils/monitoring/metrics.go`). Run `make generate-metrics-doc` after changes.

## 7. Dependency wiring

Pass dependencies as config. Open connections **in `Run`**, using the `utils/connection`
dialers, and always `defer connection.CloseConnectionsLog(conn...)`:

```go
// service/coordinator/validator_committer_manager.go:96
commonConn, err := connection.NewLoadBalancedConnection(c.clientConfig)
if err != nil {
	return fmt.Errorf("failed to create connection to validator persisters: %w", err)
}
defer connection.CloseConnectionsLog(commonConn)
vcm.commonClient = servicepb.NewValidationAndCommitServiceClient(commonConn)
```

- `NewSingleConnection` — one peer; `NewLoadBalancedConnection` — round-robin over
  endpoints; `NewConnectionPerEndpoint` — one conn per peer.
- Managers receive deps via a dedicated `*xxxManagerConfig` struct (channels, clientConfig,
  metrics, policyManager), wired in the service constructor.

## 8. Server bootstrap

Hand your `serve.Service` to `serve.StartAndServe(ctx, service, serverConfig...)`. It runs
`service.Run` in an errgroup, gates on `WaitForReady` with `ServiceStartupTimeout`, then
constructs and serves the gRPC + HTTP servers in the same group — every `g.Go` does
`defer cancel()` so any exit tears down the whole service
(`utils/serve/start_serve.go:68`).

Command wiring (cobra → read config → construct → serve),
`cmd/committer/start_cmd.go:59`:

```go
var service serve.Service
switch c := conf.(type) {
case *coordinator.Config:
	service = coordinator.NewCoordinatorService(c)
case *vc.Config:
	service = vc.NewValidatorCommitterService(ctx, c)
case *verifier.Config:
	service = verifier.New(c)
}
return serve.StartAndServe(ctx, service, serverConfig)
```

## 9. New-service checklist

1. `service/<name>/config.go` — `Config` (mapstructure + validate tags), `Default*` consts.
   Serving knobs go in `serve.Config`, not here.
2. `service/<name>/metrics.go` — `perfMetrics` embedding `*monitoring.Provider`;
   `new<X>Metrics()` using `p.New*`.
3. `service/<name>/<name>.go` — struct embedding `Unimplemented<X>Server` +
   `config`/`metrics`/`healthcheck`/`ready`; `New<X>Service(cfg)`; `Run` / `WaitForReady`
   / `RegisterService`; gRPC handlers using `grpcerror.Wrap*`. Package logger at top.
4. `cmd/config/viper.go` — register every default with `v.SetDefault`.
5. `cmd/config/app_config.go` — `Read<X>YamlAndSetupLogging` via `readYamlAndSetupLogging[<X>.Config]`.
6. `cmd/committer/config.go` + `start_cmd.go` — add the service to config dispatch and the
   `startService` switch → `serve.StartAndServe`.
7. Sample YAML under `cmd/config/samples/`. Run `make lint`, `make test`, and
   `make generate-metrics-doc`.
