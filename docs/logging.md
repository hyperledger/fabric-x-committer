<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Logging

The Fabric-X Committer uses [Hyperledger Fabric's flogging package](https://hyperledger-fabric.readthedocs.io/en/release-2.2/logging-control.html) for structured, high-performance logging. Each module declares its own named logger, enabling fine-grained per-module log level control.

## Log Levels

The following severity levels are supported, from most to least verbose (case-insensitive):

| Level     | Description                                      |
|-----------|--------------------------------------------------|
| `DEBUG`   | Detailed diagnostic information for debugging    |
| `INFO`    | General operational messages (default)           |
| `WARNING` | Potentially problematic situations               |
| `ERROR`   | Error events that might still allow continued operation |
| `PANIC`   | Severe errors that trigger a panic               |
| `FATAL`   | Critical errors; effectively disables logging when set globally |

## Configuration

Logging is configured through the `logging` section in the service YAML configuration file.

### YAML Configuration

Every service config file has a `logging` section:

```yaml
logging:
  logSpec: "info"
  format: ""
```

| Field     | Description                                                     | Default |
|-----------|-----------------------------------------------------------------|---------|
| `logSpec` | Log level specification string (see [Log Specification](#log-specification) below) | `info`  |
| `format`  | Output format; empty string uses the default console format, `json` for JSON output | `""`    |

### Environment Variable Override

Each service supports overriding the log specification via environment variables. The pattern is:

```
SC_<SERVICE>_LOGGING_LOGSPEC=<spec>
```

Where `<SERVICE>` is one of: `SIDECAR`, `COORDINATOR`, `VC`, `VERIFIER`, `QUERY`.

Examples:

```bash
# Set sidecar logging to debug
export SC_SIDECAR_LOGGING_LOGSPEC=debug

# Set coordinator logging to warning
export SC_COORDINATOR_LOGGING_LOGSPEC=warning
```

Environment variables take precedence over YAML configuration.

## Log Specification

The `logSpec` string controls both the global default level and per-module overrides. The syntax follows [Fabric's logging specification](https://hyperledger-fabric.readthedocs.io/en/release-2.2/logging-control.html#logging-specification):

```
[<module>[,<module>...]=]<level>[:[<module>[,<module>...]=]<level>...]
```

- A standalone level sets the global default for all modules.
- Override specific modules using `<module>=<level>`.
- Multiple modules can share a level: `module1,module2=debug`.
- Terms are colon-separated; order does not matter.

### Examples

| logSpec | Effect |
|---------|--------|
| `info` | All modules at INFO level |
| `debug` | All modules at DEBUG level |
| `warning:sidecar=debug` | Default WARNING, but the sidecar module at DEBUG |
| `info:coordinator,grpc-connection=debug` | Default INFO, coordinator and gRPC connection at DEBUG |
| `error:grpc-connection,broadcast-deliver=info` | Default ERROR, connection and delivery at INFO |
| `fatal` | Disables all logging (useful for benchmarks) |

### Module Names

Each module in the codebase registers a named logger. Use these names in the `logSpec` to target specific modules.

#### Sidecar

| Module Name | Description |
|-------------|-------------|
| `sidecar` | Sidecar service |
| `broadcast-deliver` | Orderer delivery client |
| `orderer-connection` | Orderer connection and identity |

#### Coordinator

| Module Name | Description |
|-------------|-------------|
| `coordinator` | Coordinator service |
| `dependencygraph` | Transaction dependency graph |

#### Verifier

| Module Name | Description |
|-------------|-------------|
| `verifier` | Verifier service |

#### Validator-Committer (VC Service)

| Module Name | Description |
|-------------|-------------|
| `validator-committer` | Validator-Committer service |
| `db-connection` | Database connection (test) |

#### Shared

| Module Name | Description |
|-------------|-------------|
| `config-reader` | Configuration reader |
| `grpc-connection` | gRPC connection utilities |
| `grpcerror` | gRPC error handling |
| `monitoring` | Metrics and monitoring |

## Output Format

The `format` field in the logging configuration controls log output format. You can use `"json"` for structured JSON output, a custom format string using the supported verbs below, or an empty string (`""`) for the default format.

### Supported Format Verbs

| Verb | Description | Optional Format |
|------|-------------|-----------------|
| `%{color}` | Level-specific SGR color escape | `reset`, `bold` (e.g., `%{color:reset}`) |
| `%{id}` | Unique log sequence number | fmt-style numeric (e.g., `%{id:04x}`) |
| `%{level}` | Log level of the entry | fmt-style string (e.g., `%{level:.4s}`) |
| `%{message}` | The log message | fmt-style string |
| `%{module}` | The logger name (module) | fmt-style string |
| `%{shortfunc}` | Name of the function creating the log record | — |
| `%{time}` | Timestamp of the log entry | Go time layout (e.g., `%{time:2006-01-02 15:04:05.000 MST}`) |

### Format Examples

```yaml
# Default console format with color, full timestamp, module, function, and level
logging:
  format: "%{color}%{time:2006-01-02 15:04:05.000 MST} [%{module}] %{shortfunc} -> %{level:.4s}%{color:reset} %{message}"
# Output: 2026-02-13 10:15:30.123 UTC [sidecar] StartService -> INFO Starting sidecar service

# Compact format with short timestamp
logging:
  format: "%{color}%{time:15:04:05.000} [%{module}] %{level:.4s}%{color:reset} %{message}"
# Output: 10:15:30.123 [sidecar] INFO Starting sidecar service

# Minimal format without color (suitable for file output)
logging:
  format: "%{time:2006-01-02 15:04:05.000 MST} [%{module}] %{level:.4s} %{message}"
# Output: 2026-02-13 10:15:30.123 UTC [sidecar] INFO Starting sidecar service

# Detailed format with sequence ID
logging:
  format: "%{time:15:04:05.000} [%{module}] %{shortfunc} %{level:.4s} %{id:04x} %{message}"
# Output: 10:15:30.123 [sidecar] StartService INFO 001a Starting sidecar service

# JSON structured output (for log aggregation systems)
logging:
  format: "json"
```

## Usage Examples

### Debugging a specific service

To debug sidecar delivery issues without flooding logs from other modules:

```yaml
logging:
  logSpec: "warning:sidecar,broadcast-deliver=debug"
```

### Production configuration

Minimal logging for production:

```yaml
logging:
  logSpec: "error:coordinator,grpc-connection=info"
```

### Quick override for troubleshooting

Override via environment variable without changing the config file:

```bash
export SC_SIDECAR_LOGGING_LOGSPEC="info:grpc-connection,broadcast-deliver=debug"
```

### Disabling logging in benchmarks

```yaml
logging:
  logSpec: "fatal"
```
