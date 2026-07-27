---
name: tests
description: Write and/or run unit and integration tests, ensuring high code coverage and reliability.
---

# Testing Code Guidelines

This document provides specific guidelines for writing and running tests in the fabric-x-committer project.

- **High Coverage Expected**: Strive for comprehensive test coverage, but focus on meaningful scenarios
- **Minimize Mocks**: Use mocks sparingly; prefer testing with real dependencies when practical
- **Database Tests**: Tests requiring databases are categorized:
    - `test-core-db`: Components that directly interact with the database
    - `test-requires-db`: Components that depend on the database layer
    - `test-no-db`: Pure logic tests with no database dependency

#### Running Database Tests

- Some tests require a database. The test harness automatically manages YugabyteDB Docker containers.
- Use `make kill-test-docker` to clean up containers after testing.
- It is best to use a local DB deployment by exporting `export DB_DEPLOYMENT=local`.
- The database is usually already running. Verify using `docker ps` and check `sc_test_postgres_unit_tests`.
- If it is not running, the database can be deployed manually using `scripts/get-and-start-postgres.sh`

#### Testing Code Guidelines

- avoid callbacks when possible - especially in tests
- helper methods at the end of the test file
- use table testing when possible to reduce code duplication
- avoid code duplication
- in table testing use `tc` as the test-case variable name
- in table testing use inline test case: `for _, tc := range []struct {...}`
- in table testing, don't use callbacks as parameters
- in table testing, split to success cases and fail cases to simplify the table tests logic.
- it is OK to create a new environment for each test case
- Use `t.Parallel()` in all tests and subtests
- Use `t.Helper()` in helper function
- In tests, never call panic. Always use `require.NoError(t, err)` to handle errors.
- Use `require.ErrorContains()` instead of `require.Error()` and then `require.Contains()`
- Address lint issues - run `make lint`

## Reusing Test Fixtures & Helpers

Don't hand-roll crypto, TLS, config-block, or service/DB setup — reuse existing helpers:

- **In-process harnesses (this repo):** `test.RunServiceForTest` / `test.ServeForTest` ([`utils/test/serve.go`](../../utils/test/serve.go)), `testdb.PrepareTestEnv` + `testdb.RunTestMain` (`utils/testdb`), and the hand-written mocks in `mock/`.
- **Proto assertions (this repo):** `test.RequireProtoEqual` / `test.RequireProtoElementsMatch` (`utils/test/require_proto.go`) — never compare protos with `require.Equal`. Both accept `require.TestingT`, so they also work inside `require.EventuallyWithT`.
- **MSP / identity / Fabric config blocks:** `github.com/hyperledger/fabric-x-common/utils/testcrypto` — `CreateOrExtendConfigBlockWithCrypto`, `ConfigBlock`, `PrepareBlockHeaderAndMetadata`, `GetPeersIdentities` / `GetConsenterIdentities` / `GetSigningIdentities` / `GetPeersMspDirs` / `GetConsenterMspDirs`.
- **TLS test material:** `github.com/hyperledger/fabric-x-common/common/crypto/tlsgen` — `tlsgen.NewCA()`, `CA`, `CertKeyPair`.
- **Generate crypto material:** `github.com/hyperledger/fabric-x-common/tools/cryptogen`.

For authoring the production code these tests cover, use the `development` skill.

## Table-Driven Tests Structure

### DO NOT Use Nested Test Groups

❌ **INCORRECT** - Do not nest success/failure cases:
```go
func TestMyFunction(t *testing.T) {
    t.Parallel()
    t.Run("success cases", func(t *testing.T) {  // ❌ Unnecessary nesting
        t.Parallel()
        for _, tc := range []struct{...}{...} {
            t.Run(tc.name, func(t *testing.T) {
                // test logic
            })
        }
    })
    t.Run("failure cases", func(t *testing.T) {  // ❌ Unnecessary nesting
        t.Parallel()
        for _, tc := range []struct{...}{...} {
            t.Run(tc.name, func(t *testing.T) {
                // test logic
            })
        }
    })
}
```

✅ **CORRECT** - Use flat structure with descriptive test names:
```go
func TestMyFunction(t *testing.T) {
    t.Parallel()
    // Success cases
    for _, tc := range []struct {
        name     string
        input    string
        expected string
    }{
        {
            name:     "valid input returns expected output",
            input:    "test",
            expected: "TEST",
        },
        {
            name:     "empty input returns empty string",
            input:    "",
            expected: "",
        },
    } {
        t.Run(tc.name, func(t *testing.T) {
            t.Parallel()
            result := MyFunction(tc.input)
            require.Equal(t, tc.expected, result)
        })
    }
    // Failure cases
    for _, tc := range []struct {
        name  string
        input string
    }{
        {
            name:  "nil input panics",
            input: nil,
        },
    } {
        t.Run(tc.name, func(t *testing.T) {
            t.Parallel()
            require.Panics(t, func() {
                MyFunction(tc.input)
            })
        })
    }
}
```

### Rationale

1. **Simpler test output**: Flat structure produces cleaner test output without extra nesting levels
2. **Easier navigation**: Test names are more discoverable in IDE test runners
3. **Less boilerplate**: Removes unnecessary wrapper functions
4. **Consistent with project style**: Matches existing test patterns in the codebase

## Table-Driven Test Best Practices

### Use Inline Test Case Definitions

✅ **CORRECT** - Define test cases inline:
```go
for _, tc := range []struct {
    name     string
    input    int
    expected int
}{
    {name: "positive number", input: 5, expected: 25},
    {name: "zero", input: 0, expected: 0},
    {name: "negative number", input: -3, expected: 9},
} {
    t.Run(tc.name, func(t *testing.T) {
        t.Parallel()
        result := Square(tc.input)
        require.Equal(t, tc.expected, result)
    })
}
```

### Variable Naming

- Use `tc` as the test case variable name in table-driven tests
- Use descriptive field names in test case structs

### Test Organization

1. **Group related tests**: Keep success and failure cases in separate loops when they have different struct fields
2. **Use descriptive names**: Test names should clearly describe what is being tested
3. **Add comments**: Use comments to separate success and failure case sections

### Parallel Execution

- Always use `t.Parallel()` in the main test function
- Always use `t.Parallel()` in each subtest
- This enables concurrent test execution for faster test runs

### Helper Functions

- Place helper functions at the end of the test file
- Use `t.Helper()` in helper functions to improve error reporting

### Error Handling in Tests

- Never call `panic()` in tests
- Always use `require.NoError(t, err)` to handle errors
- Use `require.ErrorContains(t, err, "expected message")` instead of `require.Error()` followed by `require.Contains()`

### Panic Testing

When testing functions that panic:

✅ **CORRECT** - Use `require.Panics()` for general panic testing:
```go
require.Panics(t, func() {
    MyFunction(invalidInput)
})
```

❌ **AVOID** - Don't use `require.PanicsWithValue()` or `require.PanicsWithError()` when error wrapping is involved:
```go
// This may fail due to error wrapping (e.g., cockroachdb/errors)
require.PanicsWithValue(t, "exact error message", func() {
    MyFunction(invalidInput)
})
```

**Rationale**: Error wrapping libraries (like `cockroachdb/errors`) add stack traces and metadata, making exact value matching unreliable. Use `require.Panics()` unless you have a specific need to verify the exact panic value.

## Code Duplication

- Avoid code duplication in tests
- Extract common setup logic into helper functions
- Use table-driven tests to reduce repetitive test code

## Test Coverage

- Strive for comprehensive test coverage
- Focus on meaningful scenarios rather than just achieving high coverage percentages
- Test edge cases and error conditions

## Example: Complete Test Function

```go
func TestBucketConfig_Buckets(t *testing.T) {
    t.Parallel()
    // Success cases
    for _, tc := range []struct {
        name     string
        config   BucketConfig
        expected []float64
    }{
        {
            name: "uniform distribution with 5 buckets",
            config: BucketConfig{
                Distribution: BucketUniform,
                MaxLatency:   10 * time.Second,
                BucketCount:  5,
            },
            expected: []float64{0, 2.5, 5, 7.5, 10},
        },
        {
            name: "empty distribution",
            config: BucketConfig{
                Distribution: BucketEmpty,
            },
            expected: []float64{},
        },
    } {
        t.Run(tc.name, func(t *testing.T) {
            t.Parallel()
            result := tc.config.Buckets()
            require.Equal(t, tc.expected, result)
        })
    }
    // Failure cases
    for _, tc := range []struct {
        name   string
        config BucketConfig
    }{
        {
            name: "invalid bucket count",
            config: BucketConfig{
                Distribution: BucketUniform,
                BucketCount:  0,
            },
        },
    } {
        t.Run(tc.name, func(t *testing.T) {
            t.Parallel()
            require.Panics(t, func() {
                tc.config.Buckets()
            })
        })
    }
}
```

## Metrics Testing

The project provides specialized helper functions in [`utils/test/metrics.go`](../../utils/test/metrics.go) for testing Prometheus metrics. Use these helpers consistently across all tests.

### Available Metrics Testing Functions

#### Direct Metric Value Assertions

✅ **CORRECT** - Use [`test.RequireIntMetricValue()`](../../utils/test/metrics.go:99) for immediate assertions:
```go
// Assert metric equals expected value immediately
test.RequireIntMetricValue(t, 5, myMetric)
test.RequireIntMetricValue(t, 0, myMetric.WithLabelValues("label"))

// Verify initial state
test.RequireIntMetricValue(t, 0, metrics.pendingRequests)
```

#### Eventual Metric Value Assertions

✅ **CORRECT** - Use [`test.EventuallyIntMetric()`](../../utils/test/metrics.go:105) when metrics update asynchronously:
```go
// Wait for metric to reach expected value
test.EventuallyIntMetric(
    t,
    expectedValue,
    myMetric,
    5*time.Second,        // waitFor duration
    100*time.Millisecond, // tick interval
)

// With optional message
test.EventuallyIntMetric(
    t,
    10,
    metrics.transactionsSentTotal,
    5*time.Second,
    10*time.Millisecond,
    "transactions should be sent",
)
```

#### Getting Metric Values

Use [`test.GetIntMetricValue()`](../../utils/test/metrics.go:92) or [`test.GetMetricValue()`](../../utils/test/metrics.go:69) when you need the value for custom assertions:

```go
// Get integer metric value (rounded)
preValue := test.GetIntMetricValue(t, metrics.transactionReceivedTotal)

// Perform operations...

// Assert using the captured value
test.EventuallyIntMetric(t, preValue+10, metrics.transactionReceivedTotal,
    5*time.Second, 100*time.Millisecond)

// Get float metric value (for histograms, summaries)
latency := test.GetMetricValue(t, metrics.blockMappingInRelaySeconds)
require.Greater(t, latency, float64(0))

// Use with require.EventuallyWithT for complex conditions
require.EventuallyWithT(t, func(ct *assert.CollectT) {
    require.Equal(ct, 0, test.GetIntMetricValue(t, metrics.queueSize))
    require.Equal(ct, 5, test.GetIntMetricValue(t, metrics.activeWorkers))
}, 3*time.Second, 500*time.Millisecond)
```

#### HTTP Metrics Endpoint Testing

For integration tests that check metrics via HTTP:

```go
// Check that specific metrics exist in the output
test.CheckMetrics(t, metricsURL, tlsConfig,
    "loadgen_block_sent_total",
    "loadgen_transaction_committed_total",
)

// Get specific metric value from HTTP endpoint
count := test.GetMetricValueFromURL(t, metricsURL, "metric_name", tlsConfig)
require.Greater(t, count, 500)
```

### Metrics Testing Best Practices

1. **Use Appropriate Helper**: Choose the right helper based on timing requirements:
    - [`test.RequireIntMetricValue()`](../../utils/test/metrics.go:99) for synchronous operations where the metric should already have the expected value
    - [`test.EventuallyIntMetric()`](../../utils/test/metrics.go:105) for asynchronous operations where the metric will reach the expected value after some time
    - [`test.GetIntMetricValue()`](../../utils/test/metrics.go:92) for capturing baseline values or when using with `require.Eventually()` for complex conditions
    - [`test.GetMetricValue()`](../../utils/test/metrics.go:69) for float metrics (histograms, summaries) or when you need the raw float value

2. **Capture Baseline Values**: When testing incremental changes, capture the metric value before the operation:
   ```go
   preValue := test.GetIntMetricValue(t, metrics.transactionReceivedTotal)

   // perform operation that adds 10 transactions
   sendTransactions(10)

   // Verify the increment
   test.EventuallyIntMetric(t, preValue+10, metrics.transactionReceivedTotal,
       5*time.Second, 100*time.Millisecond)
   ```

3. **Test Labeled Metrics**: Use [`WithLabelValues()`](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus#CounterVec.WithLabelValues) to test specific label combinations:
   ```go
   test.RequireIntMetricValue(t, 10,
       metrics.transactionCommittedTotal.WithLabelValues("COMMITTED"))
   test.RequireIntMetricValue(t, 2,
       metrics.transactionCommittedTotal.WithLabelValues("MALFORMED"))
   ```

4. **Use Appropriate Wait Times**: For [`test.EventuallyIntMetric()`](../../utils/test/metrics.go:105):
    - Unit tests: 1-5 seconds wait, 10-100ms tick
    - Integration tests: 5-60 seconds wait, 100ms-1s tick
    - Examples from codebase:
        - Fast operations: `5*time.Second, 10*time.Millisecond`
        - Normal operations: `5*time.Second, 100*time.Millisecond`
        - Slow operations: `2*time.Second, 200*time.Millisecond`

5. **Test Initial State**: Always verify metrics start at expected initial values:
   ```go
   test.RequireIntMetricValue(t, 0, metrics.pendingRequests)
   test.RequireIntMetricValue(t, 0, metrics.activeStreams)
   ```

6. **Verify Metric Decrements**: When testing gauges that can decrease:
   ```go
   test.RequireIntMetricValue(t, 5, metrics.queueSize)
   // process items
   test.EventuallyIntMetric(t, 0, metrics.queueSize, 2*time.Second, 100*time.Millisecond)
   ```

7. **Use `require.Never()` for Bounds Testing**: When verifying metrics don't exceed limits:
   ```go
   require.Never(t, func() bool {
       return test.GetIntMetricValue(t, metrics.counter) > maxExpected
   }, 3*time.Second, 1*time.Second)
   ```

8. **Use `require.EventuallyWithT()` for Multi-Metric Conditions**: When checking multiple metrics, always use `require.EventuallyWithT()` instead of `require.Eventually()` to ensure clear visibility of which metric failed:
   ```go
   // ✅ CORRECT - Shows which metric failed
   require.EventuallyWithT(t, func(ct *assert.CollectT) {
       require.Equal(ct, 1, test.GetIntMetricValue(t, metrics.inputQueue))
       require.Equal(ct, 1, test.GetIntMetricValue(t, metrics.outputQueue))
       require.Equal(ct, 0, test.GetIntMetricValue(t, metrics.processingQueue))
   }, 3*time.Second, 500*time.Millisecond)

   // ❌ INCORRECT - Doesn't show which condition failed
   require.Eventually(t, func() bool {
       return test.GetIntMetricValue(t, metrics.inputQueue) == 1 &&
              test.GetIntMetricValue(t, metrics.outputQueue) == 1 &&
              test.GetIntMetricValue(t, metrics.processingQueue) == 0
   }, 3*time.Second, 500*time.Millisecond)
   ```

9. **Use `require.EventuallyWithT()` for Single Metrics with Context**: When you need detailed assertion messages for a single metric:
   ```go
   require.EventuallyWithT(t, func(ct *assert.CollectT) {
       actual := test.GetIntMetricValue(t, metrics.counter)
       require.Equal(ct, expected, actual, "counter should reach expected value")
   }, 5*time.Second, 100*time.Millisecond)
   ```

10. **Test Timing Metrics**: For histogram and summary metrics, verify they are positive:
    ```go
    latency := test.GetMetricValue(t, metrics.requestDurationSeconds)
    require.Greater(t, latency, float64(0))
    ```

### Common Patterns

#### Pattern 1: Incremental Counter Testing
```go
// Capture baseline
preValue := test.GetIntMetricValue(t, metrics.transactionReceivedTotal)

// Perform operation
sendTransactions(5)

// Verify increment
test.EventuallyIntMetric(t, preValue+5, metrics.transactionReceivedTotal,
    5*time.Second, 100*time.Millisecond)
```

#### Pattern 2: Multiple Labeled Metrics
```go
test.RequireIntMetricValue(t, 30, metrics.notifierPendingTxIDs)
test.RequireIntMetricValue(t, 6, metrics.notifierUniquePendingTxIDs)
test.RequireIntMetricValue(t, 10, metrics.notifierTxIDsStatusDeliveries)
test.RequireIntMetricValue(t, 0, metrics.notifierTxIDsTimeoutDeliveries)
```

#### Pattern 3: Queue Size Monitoring
```go
// Verify queue fills up
test.EventuallyIntMetric(t, 3, metrics.waitingTransactionsQueueSize,
    5*time.Second, 10*time.Millisecond)

// Process items
processQueue()

// Verify queue empties
test.EventuallyIntMetric(t, 0, metrics.waitingTransactionsQueueSize,
    5*time.Second, 10*time.Millisecond)
```

#### Pattern 4: Complex Multi-Metric Conditions
```go
// Use EventuallyWithT for clear failure messages
require.EventuallyWithT(t, func(ct *assert.CollectT) {
    require.Equal(ct, 10, test.GetIntMetricValue(t, metrics.gdgTxProcessedTotal))
    require.Equal(ct, 8, test.GetIntMetricValue(t, metrics.gdgValidatedTxProcessedTotal))
}, 2*time.Second, 200*time.Millisecond)
```

#### Pattern 5: Integration Test HTTP Metrics
```go
require.EventuallyWithT(t, func(ct *assert.CollectT) {
    count := test.GetMetricValueFromURL(
        t, metricsURL, "loadgen_transaction_committed_total", tlsConfig,
    )
    require.Greater(ct, count, 500)
}, 300*time.Second, 1*time.Second)
```

### Example: Complete Metrics Test

```go
func TestRelayMetrics(t *testing.T) {
    t.Parallel()

    env := setupTestEnvironment(t)
    m := env.metrics

    // Verify initial state
    test.RequireIntMetricValue(t, 0, m.transactionsSentTotal)
    test.RequireIntMetricValue(t, 0, m.waitingTransactionsQueueSize)

    // Submit transactions
    txCount := 3
    submitTransactions(txCount)

    // Verify metrics update asynchronously
    test.EventuallyIntMetric(t, txCount, m.transactionsSentTotal,
        5*time.Second, 10*time.Millisecond)
    test.EventuallyIntMetric(t, txCount, m.waitingTransactionsQueueSize,
        5*time.Second, 10*time.Millisecond)

    // Process transactions
    processTransactions()

    // Verify labeled metrics
    test.RequireIntMetricValue(t, txCount,
        m.transactionsStatusReceivedTotal.WithLabelValues("COMMITTED"))

    // Verify queue empties
    test.EventuallyIntMetric(t, 0, m.waitingTransactionsQueueSize,
        5*time.Second, 10*time.Millisecond)

    // Verify timing metrics are positive
    require.Greater(t, test.GetMetricValue(t, m.processingSeconds), float64(0))
}
```

## Summary

- **No nested test groups** - Keep table-driven tests flat
- **Use `tc` for test case variable**
- **Always use `t.Parallel()`**
- **Use `require.Panics()` for panic testing**
- **Descriptive test names**
- **Helper functions at the end with `t.Helper()`**
- **Never use `panic()` in tests**
- **Use metrics testing helpers** - [`test.RequireIntMetricValue()`](../../utils/test/metrics.go:99), [`test.EventuallyIntMetric()`](../../utils/test/metrics.go:105), [`test.GetIntMetricValue()`](../../utils/test/metrics.go:92)
