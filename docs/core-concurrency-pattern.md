<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Core Concurrency Pattern

   1. [errgroup - Synchronization, Error Propagation, and Context Cancellation for Goroutines](#errgroup-synchronization,-error-propagation,-and-context-cancellation-for-goroutines) - [Code](https://cs.opensource.google/go/x/sync)
   2. [channel - Handling Communication within and across errgroup Tasks](#handling-channel-communication-within-errgroup-tasks) - [Code](/utils/channel/channel.go)

## 1. errgroup - Synchronization, Error Propagation, and Context Cancellation for Goroutines

The `errgroup` package, part of the `golang.org/x/sync`
collection, provides a convenient way to manage a group of goroutines working on subtasks of a
common task. It handles synchronization (waiting for all goroutines to finish), propagates the
first error encountered, and integrates with Go's `context` package for cancellation.

### What it Does

Imagine you need to perform several independent tasks concurrently, like fetching data from multiple
URLs or processing different parts of a file in parallel. You want to:

1.  **Run tasks concurrently:** Launch multiple goroutines.
2.  **Wait for completion:** Ensure your main program waits until all these goroutines have finished.
3.  **Handle errors:** If any of the goroutines encounters an error, you want to know about it. Crucially, `errgroup` 
    captures and returns only the first non-nil error returned by any goroutine.
4.  **Cancel gracefully:** If one goroutine fails (returns an error), or if the parent operation is cancelled 
    (e.g., via a timeout or user request), you often want to signal the other running goroutines to stop work
    early. `errgroup` handles this using Go's `context` package.

`errgroup` provides a clean abstraction to manage these common requirements.

### Key Features

* **Concurrent Execution:** Easily launch multiple functions as goroutines using the `Go` method.
* **Synchronization:** The `Wait` method blocks until all launched goroutines have completed.
* **First Error Propagation:** `Wait` returns the *first* non-nil error returned by any of the goroutines launched
  via `Go`. If all goroutines succeed (return `nil`), `Wait` returns `nil`.
* **Context Integration & Cancellation:** An `errgroup.Group` is associated with a `context.Context`. 
  If one goroutine returns an error, or if the parent context is cancelled, the context associated
  with the group is cancelled. Other running goroutines can check this context (`ctx.Done()` or
  `ctx.Err() != nil`) to stop processing early.

### How We Use it to Manage Long-Running Interdependent Tasks
A common and powerful use case for errgroup is managing a set of long-running
tasks that depend on each other to function correctly as a unit.
Examples include network service listeners or resource monitors that should all 
run together.

These tasks are typically intended to run indefinitely until a critical
event occurs, such as a lost network connection or an unrecoverable internal 
error within one of the tasks. The key requirement in this scenario is: if any 
single task in the group fails and terminates, all other tasks belonging 
to that same logical group should also be terminated promptly.

The errgroup facilitates this pattern effectively:

1. Each long-running task is launched within `g.Go(func() error { ... })`, 
   where `g` is an `errgroup.Group` created with errgroup.WithContext.

2. The function for each task is designed to run continuously (e.g., in a loop)
   and only return a non-nil error upon encountering a fatal, non-recoverable condition. 

3. Normal successful completion might never occur.

4. Each running task must actively monitor the context (ctx) associated with the group.
   This is typically done using either a select statement checking `ctx.Done()` or `for ctx.Err() != nil`.

5. When the first goroutine returns a non-nil error (signifying a fatal failure), errgroup 
   automatically cancels the shared context (ctx).

6. This cancellation signal is detected by the other running goroutines in their select statements
   using `ctx.Done()` or in `for ctx.Err() != nil`.

7. Upon detecting cancellation, these other goroutines should perform necessary cleanup 
   and then return, usually by returning `ctx.Err()`.

8. The main `g.Wait()` call, which was blocking, will eventually unblock only after all 
   goroutines (the one that failed and all the ones that were cancelled) have returned.
   `g.Wait()` will return the original fatal error that triggered the shutdown cascade.

This pattern ensures that related long-running tasks are managed as a cohesive unit, 
reliably shutting down together when any essential part fails, preventing partially 
functioning states.

Example from [service/sidecar/relay.go](/service/sidecar/relay.go#L64)

```go
  g, gCtx := errgroup.WithContext(stream.Context())
  g.Go(func() error {
    return r.preProcessBlockAndSendToCoordinator(gCtx, stream, config.configUpdater)
  })

  statusBatch := make(chan *protoblocktx.TransactionsStatus, 1000)
  g.Go(func() error {
    return receiveStatusFromCoordinator(gCtx, stream, statusBatch)
  })

  g.Go(func() error {
    return r.processStatusBatch(gCtx, statusBatch)
  })

  g.Go(func() error {
    return r.setLastCommittedBlockNumber(gCtx, config.coordClient, expectedNextBlockToBeCommitted)
  })

  return errors.Wrap(g.Wait(), "stream with the coordinator has ended")
```

## 2. channel - Handling Communication within and across errgroup Tasks

Our long-running tasks, managed by `errgroup`, often need to communicate using channels,
frequently implementing producer-consumer patterns. A critical challenge arises when using
standard Go channels in conjunction with `errgroup`'s context cancellation mechanism.

### The Problem

If one goroutine within the `errgroup` fails, `errgroup` cancels the shared context (`ctx`). However,
other goroutines in the group might be blocked indefinitely on standard channel operations:

 - A read operation (`<-ch`) can block forever if the channel is empty and the producing 
   goroutine has already terminated due to the context cancellation.
 - A write operation (`ch <- data`) can block forever if the channel is full and the 
   consuming goroutine has already terminated due to the context cancellation.

This blocking prevents the waiting goroutine from observing the context cancellation 
(`ctx.Done()`) promptly, thereby hindering the clean and complete shutdown of all interdependent
tasks managed by the `errgroup`.

### Our Solution: Context-Aware Channel Wrappers

To prevent these deadlocks and ensure reliable shutdown, we utilize a custom `channel` package.
This package provides wrappers (like `Reader`, `Writer`, `ReaderWriter`) around standard Go channels.

The key feature of these wrappers is that they integrate context awareness directly into the channel
read and write operations. Internally, methods like `Read` or `Write` use a `select` statement that
simultaneously attempts the underlying channel operation and listens for context 
cancellation (`case <-ctx.Done()`).

### The Benefit

If the `errgroup` context is cancelled while a goroutine is waiting on a channel operation using our wrapper, 
the operation unblocks immediately (typically returning an error indicating cancellation, 
like `context.Canceled` or `ctx.Err()`). This "quick release" allows the goroutine to promptly 
detect and respect the cancellation signal, perform any necessary cleanup, and exit cleanly.

By consistently using this helper package for inter-task communication instead of raw Go channels, 
we ensure that our system never hangs on channel operations during shutdown and that all goroutines 
managed by the `errgroup` can terminate reliably when required.
