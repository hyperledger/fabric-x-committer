<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

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
- **Testing**: Standard Go testing with Docker-based integration tests
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

# Build in Docker container (useful for consistent builds)
make build-with-docker

# Build for specific OS/architecture
make build os=linux arch=amd64

# Build release binaries for multiple architectures
make build-release
```

### Testing

```bash
# Run unit tests (excludes integration and container tests)
make test

# Run integration tests
make test-integration

# Run tests with coverage
make test-cover

# Run specific package tests
make test-package-<package-name>

# Run container tests (requires Docker images)
make test-container
```

#### Database Tests

- Some tests require a database. The test harness automatically manages YugabyteDB Docker containers.
- Use `make kill-test-docker` to clean up containers after testing.
- It is best to use a local DB deployment by exporting `export DB_DEPLOYMENT=local`.
- The database is usually already running. Verify using `docker ps` and check `sc_test_postgres_unit_tests`.
- If it is not running, the database can be deployed manually using `scripts/get-and-start-postgres.sh`

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

### Code Philosophy: Simplicity First

This project strongly emphasizes **simple, readable code over clever abstractions**. Key principles from `guidelines.md`:

1. **Avoid Premature Abstraction**: Don't introduce interfaces, generics, or complex patterns unless there's a clear, compelling need
2. **Minimize Interfaces**: Use concrete types by default; interfaces only for truly pluggable architectures
3. **Limit Generics**: Use sparingly for simple utility functions; avoid for core business logic
4. **Avoid Function Arguments**: Don't pass functions as callbacks; prefer interfaces with the Strategy Pattern if pluggability is needed
5. **Explicit Over Clever**: Favor explicit, slightly longer code over compact but hard-to-understand solutions

### Error Handling

The project uses `github.com/cockroachdb/errors` for comprehensive error handling:

1. **Capture Stack Traces at Origin**: Use `errors.New`, `errors.Newf`, `errors.Wrap`, or `errors.Wrapf` when first encountering an error
2. **Add Context Without Duplicating Traces**: Use `fmt.Errorf` with `%w` to add context while preserving the original stack trace
3. **Log Full Details at Exit Points**: Use `logger.ErrorStackTrace(err)` at service boundaries (e.g., gRPC handlers)
4. **Convert to gRPC Errors**: At gRPC boundaries, convert internal errors to appropriate status codes using helpers in `utils/grpcerror`

#### Code Guidelines

- avoid callbacks when possible
- avoid code duplication
- Collate methods of the same struct togather
- Order the methods such that the method user is before the method declaration
- Address lint issues - run `make lint`

### Code Review Standards

Reviews use three priority levels:

- **Major**: Critical issues affecting functionality, performance, or maintainability (must be addressed)
- **Minor**: Code style, naming, or best practices (should be addressed)
- **Nit**: Typos, formatting, minor preferences (optional)

Always be polite and constructive in reviews. Use "we" instead of "you" to frame issues as team problems.

### Performance Considerations

- Make informed choices about algorithms and data structures based on known performance characteristics
- Use benchmarking (`make bench-*` targets) to validate performance assumptions
- Perform load testing in production-like environments to identify real bottlenecks
- Only optimize after profiling shows a genuine issue

## Project Structure

```
/
├── api/                    # Protocol Buffer definitions
│   └── servicepb/         # Service protobuf files
├── cmd/                   # Main applications
│   ├── committer/         # Main committer service
│   ├── loadgen/           # Load generation tool
│   ├── mock/              # Mock services for testing
│   └── config/            # Configuration utilities and samples
├── service/               # Core service implementations
│   ├── coordinator/       # Transaction orchestration
│   ├── vc/               # Validator-Committer service
│   ├── sidecar/          # Block fetching middleware
│   ├── query/            # Query service
│   └── verifier/         # Signature verification
├── loadgen/              # Load generation framework
├── mock/                 # Mock implementations
├── utils/                # Shared utilities
├── integration/          # Integration tests
├── docker/               # Docker configurations
│   ├── images/          # Dockerfiles
│   └── deployment/      # Docker Compose setups
├── docs/                 # Detailed documentation
└── scripts/              # Build and utility scripts
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

7. **Linting**: Run `make lint` before submitting PRs. The project uses golangci-lint with strict rules.

8. **Code Ownership**: Check `CODEOWNERS` file for component ownership. Code owners must review changes to their areas.

## Common Development Tasks

### Adding a New Service

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
- [ ] `make test` passes all tests
- [ ] Benchmarks run if performance-critical code changed

## Getting Help

- Review detailed service documentation in `docs/`
- Check `guidelines.md` for coding standards and philosophy
- Examine existing code for patterns and conventions
- Run `make` to see all available build targets

**Maintenance**:
- Update when new conventions are established
- Add new patterns as they emerge
- Keep synchronized with `guidelines.md` and project evolution
- Document any project-specific decisions or rationale
