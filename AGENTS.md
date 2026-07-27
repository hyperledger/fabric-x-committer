This file provides guidance to agents when working with code in this repository.

## Project Overview

**Fabric-X Committer** is a high-performance transaction validation and commitment system for Hyperledger Fabric blockchain networks. It implements a distributed, pipelined architecture that processes transactions through multiple stages: signature verification, dependency analysis, MVCC validation, and database commitment.

### Core Architecture

The system consists of four main microservices that communicate via gRPC:

1. **Sidecar**: Fetches blocks from the Hyperledger Fabric ordering service, performs initial validation, and delivers committed blocks to clients
2. **Coordinator**: Orchestrates the transaction pipeline using a dependency graph to maximize parallel processing while ensuring deterministic outcomes
3. **Signature Verifier**: Validates transaction signatures and structural integrity
4. **Validator-Committer (VC)**: Performs MVCC validation and commits valid transactions to the state database (PostgreSQL/YugabyteDB)

### Key Technologies

- **Language**: Go 1.26
- **Communication**: gRPC with Protocol Buffers
- **Database**: PostgreSQL or YugabyteDB (for state storage)
- **Testing**: Standard Go testing with binaries integration tests and Docker-based integration tests
- **Build**: Make-based build system with Docker support

## Building and Running

### Prerequisites

- Go 1.26 or later
- Docker (for integration tests and containerized builds)
- PostgreSQL or YugabyteDB (for tests requiring database)

### Build Commands

```bash
# Build all binaries locally
make build
```

### Running Services

Each service has its own binary in `cmd/`:

```bash
# Run committer (main service)
./bin/committer --config <config-file>

# Run load generator
./bin/loadgen --config <config-file>

# Run mock services (for testing)
./bin/mock --config <config-file>
```

Configuration samples are available in `cmd/config/samples/`.

## Development Conventions

Follow the **`development` skill** whenever you write or change Go code — it is the
authoritative, detailed source for these conventions and keeps them in one place. In brief:

- **Simplicity first** — concrete types over interfaces, generics only for `utils/`-style
  plumbing, no callbacks/functions-as-arguments, explicit over clever (`guidelines.md`).
- **Error handling** — `errors.New/Newf/Wrap/Wrapf` (`cockroachdb/errors`) at the origin,
  `fmt.Errorf("...%w", err)` to add context, `logger.Errorf("%+v", err)` at boundaries, and
  `utils/grpcerror` at gRPC edges.
- **Code organization** — collate a struct's methods together; place a method above the
  functions it calls (caller before callee).
- **Reuse `utils/` and `fabric-x-common` helpers**; run `make lint` (or `make lint-go`).

See the skill for concurrency (`errgroup` + `utils/channel`), Go 1.26 idioms,
service/config/metrics structure, and the full linter cheat sheet.

### Performance Considerations

- Make informed choices about algorithms and data structures based on known performance characteristics
- Use benchmarking (`make bench-*` targets) to validate performance assumptions
- Perform load testing in production-like environments to identify real bottlenecks
- Only optimize after profiling shows a genuine issue

## Project Structure

```
/
├── api/                 # Protocol Buffer definitions
├── cmd/                 # Main applications
│   ├── committer/       # Main committer service
│   ├── loadgen/         # Load generation tool
│   ├── mock/            # Mock services for testing
│   └── config/          # Configuration utilities and samples
├── service/             # Core service implementations
│   ├── coordinator/     # Transaction orchestration
│   ├── vc/              # Validator-Committer service
│   ├── sidecar/         # Block fetching middleware
│   ├── query/           # Query service
│   └── verifier/        # Signature verification
├── loadgen/             # Load generation framework
├── mock/                # Mock implementations
├── utils/               # Shared utilities
├── integration/         # Integration tests
├── docker/              # Docker configurations
│   ├── images/          # Dockerfiles
│   └── deployment/      # Docker Compose setups
├── docs/                # Detailed documentation
└── scripts/             # Build and utility scripts
```

## Key Documentation

Detailed architectural documentation is available in the `docs/` directory:

- `docs/setup.md`: Prerequisites and quick start guide
- `docs/coordinator.md`: Coordinator service architecture and workflow
- `docs/validator-committer.md`: VC service internals and database schema
- `docs/sidecar.md`: Sidecar middleware design and recovery
- `docs/verification-service.md`: Signature verification details
- `docs/query-service.md`: Query service API
- `docs/logging.md`: Logging conventions
- `docs/metrics_reference.md`: Available metrics
- `docs/tls-configurations.md`: TLS setup

## Important Notes for Agents

1. **Database Schema Management**: The VC service manages database tables and stored procedures. Changes to schema require careful coordination.
2. **Concurrency Patterns**: The system uses extensive pipelining with Go channels. Understand the flow: `Sidecar → Coordinator → Signature Verifier → VC Service`
3. **Idempotency**: Services are designed to handle duplicate transactions gracefully. The VC service checks for existing transaction statuses before committing.
4. **Dependency Graph**: The Coordinator uses a DAG to track transaction dependencies (read-write, write-read, write-write) to enable parallel processing.
5. **Recovery Design**: All services support graceful recovery. The Sidecar and Coordinator track progress in the state database for restart scenarios.
6. **Protocol Buffers**: Changes to `.proto` files require regeneration: `make proto`
7. **Code Ownership**: Check `CODEOWNERS` file for component ownership. Code owners must review changes to their areas.

## Common Development Tasks

### Adding a New Service

See the `development` skill's service-construction reference for the full blueprint
(struct + constructor, `serve.Service` lifecycle, gRPC handlers, config pipeline, metrics,
wiring) with a step-by-step checklist. High level:

1. Define protobuf service in `api/servicepb/`
2. Generate code: `make proto`
3. Implement service in `service/<name>/`
4. Add configuration in `cmd/config/`
5. Add tests (unit and integration)
6. Update documentation

### Modifying Database Schema

1. Update stored procedures in `service/vc/`
2. Update schema documentation in `docs/validator-committer.md`
3. Add migration logic if needed
4. Test with both PostgreSQL and YugabyteDB

### Adding Metrics

1. Define metrics in appropriate service package
2. Register with Prometheus
3. Update `docs/metrics_reference.md`: `make generate-metrics-doc`

### Performance Optimization

1. Write benchmark: `go test -bench=. ./...`
2. Profile: Use `go test -cpuprofile` or `go test -memprofile`
3. Optimize based on data
4. Verify improvement with benchmarks
5. Document trade-offs in code comments

## Pre-Pull Request Checklist

Before submitting a PR:

- [ ] Code works correctly and handles edge cases
- [ ] Code is readable with clear naming and structure
- [ ] Follows project conventions and patterns
- [ ] Tests cover key functionality and edge cases
- [ ] Documentation/comments explain complex logic
- [ ] `make lint` passes without errors
- [ ] Relevant unit/integration tests pass locally (don't run the full `make test` locally — too slow); rely on CI for the full suite and monitor it
- [ ] Benchmarks run if performance-critical code changed

## Getting Help

- Review detailed service documentation in `docs/`
- Examine existing code for patterns and conventions
- Run `make` to see all available build targets
