<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Testing Code Guidelines

This document provides specific guidelines for writing tests in the fabric-x-committer project.

- **High Coverage Expected**: Strive for comprehensive test coverage, but focus on meaningful scenarios
- **Minimize Mocks**: Use mocks sparingly; prefer testing with real dependencies when practical
- **Database Tests**: Tests requiring databases are categorized:
    - `test-core-db`: Components that directly interact with the database
    - `test-requires-db`: Components that depend on the database layer
    - `test-no-db`: Pure logic tests with no database dependency

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

## Summary

- **No nested test groups** - Keep table-driven tests flat
- **Use `tc` for test case variable**
- **Always use `t.Parallel()`**
- **Use `require.Panics()` for panic testing**
- **Descriptive test names**
- **Helper functions at the end with `t.Helper()`**
- **Never use `panic()` in tests**
