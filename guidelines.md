<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Table of Contents

1. [Labeling Review Comments](#labeling-review-comments)
2. [The Importance of Politeness in Code Reviews](#the-importance-of-politeness-in-code-reviews)
3. [GoLang Coding Standards](#golang-coding-standards)
   - [Error Handling](#error-handling)
4. [The Arguments for Simplicity](#the-argument-for-simplicity)
   - [Prioritize Simple and Readable Code over "Clever" Code](#prioritize-simple-and-readable-code-over-clever-code)
   - [Avoid Premature Optimization](#avoid-premature-optimization)
   - [Apply YAGNI (You Ain't Gonna Need It)](#apply-yagni-you-aint-gonna-need-it)
5. [The Downsides and Complexities Introduced By Interfaces, Generics, and Function Arguments](#the-downsides-and-complexities-introduced-by-interfaces-generics-and-function-arguments)
   - [Interfaces (Layers of Indirection and Obscurity)](#interfaces-layers-of-indirection-and-obscurity)
   - [Generics (complexity and obfuscation)](#generics-complexity-and-obfuscation)
   - [Passing Functions as Arguments (Destroyed Control Flow and Debugging Nightmares)](#passing-functions-as-arguments-destroyed-control-flow-and-debugging-nightmares)
8. [Test Coverage and Use of Mocks](#test-coverage-and-use-of-mocks)
9. [Performance Considerations](#performance-considerations)
10. [Code Owners](#code-owners)
11. [Pre-Pull Request Checklist](#pre-pull-request-checklist)

# Labeling Review Comments

When reviewing this project, comments can be categorized into three levels of importance:

 1. Major
 2. Optional
 3. Nit.

Adding one of these labels to each review comment help communicate the significance and urgency.

## Major Comments

These comments address significant issues that could affect the program's functionality, performance,
or maintainability. Major comments might discuss things like architectural decisions, algorithmic errors,
data structure choices, significant performance bottlenecks, or violations of important programming principles.
They could also point out when there's a lack of necessary testing or if critical bugs have been introduced.

**Examples for Major Label:**

1. Usage of `time.Sleep()` often results in flaky test. Please consider using `require.Eventually()`
2. Usage of parent and multi-child contexts make the code very complex and affect the long term maintainablility
   of the code. Please keep it simple and straightforward.
3. Usage of multiple go modules within a project would make a lot of issues during release and dependency
   management. Hence, please use single go module.
4. This recursive function doesn't seem to have a clear base case, which could lead to a stack overflow.
   Please ensure the recursion terminates under the correct conditions.
5. There's no error handling after the database call on line 34. This could cause the application to crash
   if the database operation fails. Please add appropriate error handling.
6. This `deleteRecord()` function doesn't seem to check if the record exists before attempting deletion.
   This could result in a runtime error. Please add a check for the existence of the record before deleting.
7. The `retrieveData()` function doesn't seem to close the database connection once the data retrieval is done.
   This might lead to connection leaks. Ensure to close the connection in a deferred statement or after usage.

## Minor Comments

Minor comments, though less critical than Major comments, still contribute to improving the overall quality
of the code. These comments usually refer to code style, naming conventions, and best practices. They are not
strictly necessary but help to improve code readability and consistency.

**Examples for Minor Label:**

1. The variable name `target` can be renamed to `reachableAddress` for better readability. Please consider
   using explicit names.
2. This function looks too big. Can you please consider to split them into moderate sized functions?
3. Add a TODO or a comment here.
4. This block of code is repeated in several places. Consider refactoring it into a helper function for
   better maintainability and reusability.
5. Use `t.Logf()` instead of `log.Fatal()` as the former outputs additional information related to the test.
6. It seems like you're using raw strings to construct the SQL query. Consider using a query builder or
   an ORM to make the code cleaner and easier to read.
7. This function has quite a few parameters, which could make it difficult to use and maintain. Consider
   introducing a parameter object.
8. The order of the functions in this file seems arbitrary. Organizing them in a logical order, such as
   by their usage or in the order they're called, could improve code readability.

## Nit Comments

Nit comments, short for "nitpicks", are minor suggestions that address small details such as formatting,
grammar in comments, or the arrangement of code. These comments don't impact the functionality or performance
of the code and are more about personal preference or minor style guide adherence.

**Examples for Nit Label:**

1. There's a typo in the comment on line 17. "Retruns" should be "Returns".
2. Please remove the trailing whitespaces on line 15.
3. There's an unnecessary empty line on line 42. Removing it would improve code compactness.
4. The import statements are not grouped properly. According to Go's convention, they should be grouped in
   the order of standard library imports, third-party imports, and then project-specific imports.
5. The indentation seems off on lines 20-25. Consistent indentation helps improve the readability of the code.
6. On line 10, consider removing the explicit 'else' after the 'if' block ends with a 'return'. It's not
   needed and removing it can make the code cleaner.
7. There are multiple nested `if-else` blocks here which make the function hard to read. Consider using
   a `switch` statement to simplify the code.

# The Importance of Politeness in Code Reviews

Code reviews are a crucial part of the development process. They help maintain code quality, share knowledge
among team members, and foster a culture of collective ownership. While the primary goal of a code review is to
identify and fix issues in the code, it's equally important to do so in a respectful and constructive manner.

When conducting code reviews, always remember that there's a human on the other side of the screen who has put
in significant effort and thought into their work.

Being polite in code reviews encourages open communication and a more collaborative environment. Here are a
few guidelines to ensure politeness:

1. Use a respectful tone: Avoid using harsh or overly critical language. If you have to point out a mistake,
   do it in a respectful manner. Phrases like "Did you consider...?", "What do you think about...?" or
   "Could we...?" sound more collaborative and less accusatory.
2. Provide clear explanations: If you suggest a change, explain why you believe it's necessary. This gives
   context and helps the recipient understand your point of view.
3. Acknowledge good work: Don't just focus on what's wrong with the code. If you come across well-written
   or clever pieces of code, acknowledge them. A little bit of positive reinforcement goes a long way in
   maintaining a healthy team morale.
4. Use 'we' instead of 'you': Framing comments as a collective problem rather than an individual mistake
   can make reviews feel more like a team effort. For instance, saying "We need to handle this error" instead
   of "You forgot to handle this error" sounds less personal and more cooperative.

Remember, the goal of code reviews is not just to improve the code, but also to help developers learn and grow.
Polite, respectful, and constructive feedback plays a key role in achieving this goal.

# GoLang Coding Standards

As part of our commitment to quality, readability, and maintainability, our GoLang projects should adhere to
established coding standards and best practices. It's not just about writing code that works—it's about
writing code that's understandable, easy to modify, and follows a consistent style that all team members can
easily work with.

Several resources provide excellent guidelines for writing idiomatic and efficient Go code. When writing or
reviewing code, you should keep these guidelines in mind and refer to them as needed:

1. [Effective Go](https://golang.org/doc/effective_go): This is an official resource provided by the Go
   project team. It provides tips and best practices for writing clear and idiomatic Go code.
2. [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments): This resource is a
   collection of common issues and suggestions for addressing them. These comments have been collected over
   many code reviews within the Go team and provide invaluable insight.
3. [The Go Programming Language Specification](https://golang.org/ref/spec): Understanding the official
   specification of the Go programming language is fundamental to writing correct Go code.
4. [Uber's Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md): This style guide from
   Uber demonstrates how a large organization applies Go best practices. It's a useful reference for
   structuring and formatting your Go code.

## Error Handling

Consistent and effective error handling is crucial for maintaining robust and Diagnosable services.
These guidelines outline our approach, primarily leveraging the `github.com/cockroachdb/errors` 
package and standard Go practices for clarity and detailed diagnostics.

1.  **Capture Stack Trace at Origin:** When an error is first encountered (i.e., received from an 
    external package like a database driver or HTTP client) or when a new error condition is created 
    within our internal packages, it's essential to capture a stack trace at that point. 
    Use `errors.New`, `errors.Newf`, `errors.Wrap`, or `errors.Wrapf` from the `github.com/cockroachdb/errors` package. 
    This establishes the precise origin point of the error.
    * **Example (External):** 
        ```go
        _, err := db.Exec(...) ; 
        if err != nil { 
          return errors.Wrap(err, "failed to execute db query") 
        }
        ```
    * **Example (Internal):** 
        ```go
        if !isValid { 
          return errors.Newf("validation failed for input field %d", X) 
        }
          ```

2.  **Decorate Errors Without Duplicating Traces:** As an error propagates up the call stack, you might 
    need to add more context or hints about the operation being performed at that level. 
    Adding context is optional; only do so if it genuinely adds value for understanding or debugging
    the error. If you choose to add context, use the standard Go `fmt.Errorf` with the `%w` verb for 
    this: `fmt.Errorf("additional context: %w", err)`. This wraps the underlying error (`err`) and 
    adds your hint, but crucially, it preserves the original stack trace captured by `cockroachdb/errors` 
    without adding a new, redundant trace. This prevents bloating the stack trace information while
    enriching the error context.
    * **Example:** 
        ```go
        if err := processItem(item); err != nil { 
          return fmt.Errorf("failed to process item %d: %w", item.ID, err) 
        }
        ```

3.  **Log Full Error Details at Exit Points:** At the boundaries or exit points of logical operations
    (e.g., within a gRPC handler just before returning a response, or finishing a background task), log the 
    complete error details. This *must* include the full stack trace. Use `logger.Errorf("%+v", err)`:
    the `%+v` verb extracts and formats the stack trace contained
    within the `cockroachdb/errors` error object.

4.  **Handle Errors at gRPC Boundaries:** You cannot directly return stack traces over a gRPC connection.
    Instead, after logging the detailed internal error (as per point 3), convert the error into an 
    appropriate gRPC status code. Use helper functions, preferably located in `utils/grpcerror`, to map 
    internal error types or conditions to standard gRPC codes (e.g., `codes.Internal` for unexpected 
    server errors, `codes.InvalidArgument` for validation failures, `codes.NotFound`, etc.). This provides
    the client with a meaningful, standardized error without exposing internal implementation details
    or verbose stack traces.
    * **Example (gRPC Handler):**
        ```go
        func (s *server) MyGRPCHandler(ctx context.Context, req *pb.Request) (*pb.Response, error) {
            res, err := s.service.DoSomething(ctx, req.Data)
            if err != nil {
                logger.Errorf("%+v", err) // Log the full error + stack trace internally
                // Convert to gRPC status error for the client
                return nil, grpcerror.WrapInternalError(err) 
            }
            return &pb.Response{Result: res}, nil
        }
        ```
# The Argument for Simplicity

Simple code is generally:

1. *Easier to Read*: Follows a predictable path.
2. *Easier to Understand*: Less indirection and abstraction mean the intent and execution are clearer.
3. *Easier to Debug*: Errors are easier to trace when the execution path is linear.
4. *Easier to Maintain*: Modifications are often more localized and less likely to have unforeseen
  consequences across disparate parts of the codebase via complex contracts or distributed behavior.

Introducing interfaces, generics, and higher-order functions adds layers of complexity that, while 
sometimes necessary, invariably make the code harder to read, navigate, and maintain compared to a 
simpler, more direct implementation. They raise the barrier to entry for understanding and 
contributing to the codebase.

## Prioritize Simple and Readable Code over "Clever" Code

1. *Guideline*: Always favor code that is simple, explicit, and easy to understand over code that is overly concise
   or uses "clever" tricks. Aim for code that a new team member could understand quickly without needing deep context
   or specialized language knowledge. When faced with a choice between simple/longer and clever/shorter, default to simple.
2. *Rationale*: Code is read far more often than it is written. While clever solutions might seem elegant or efficient
   at first glance, they often increase the cognitive load required to understand, debug, and modify them later. 
   Maintainability and readability are paramount for the long-term health of the codebase.   
3. *Acceptable Trade-off*: It is perfectly acceptable for simple, straightforward code to require a few extra lines
   compared to a more compact but complex ("clever") alternative. For example, choosing an explicit `if/else` structure 
   or breaking down a complex operation into intermediate variables might add 5-10 lines but drastically improve clarity. 
   This trade-off is worthwhile if it makes the code's intent immediately obvious.

## Avoid Premature Optimization

1. *Guideline*: Write code that is clear and correct first. Only optimize when performance profiling indicates a 
   genuine bottleneck.
2. *Rationale*: Optimizations often involve writing more complex, less readable code ("clever" code). Such efforts 
   are wasted if there isn't a real performance issue, and they increase the maintenance burden. This aligns directly
   with the "Simple over Clever" principle.
3. *Process*: 1. Make it work. 2. Make it right (clear, maintainable). 3. Then, if necessary based on data, make it fast.

## Apply YAGNI (You Ain't Gonna Need It)

1. *Guideline*: Do not implement functionality or add abstractions based on anticipated future needs. Focus only on the
   requirements that exist now.
2. *Rationale*: Building speculative features adds complexity and maintenance overhead for code that might never be used
   or might need to be implemented differently when the actual requirement arises. This directly supports avoiding
   unnecessary interfaces, generics, or complex structures.

# The Downsides and Complexities Introduced By Interfaces, Generics, and Function Arguments

While often touted for flexibility and reuse, interfaces, generics, and function arguments can significantly
hinder readability, complicate maintenance, and make code navigation a chore.

## Interfaces (Layers of Indirection and Obscurity)

1. *Readability*: Interfaces introduce indirection. With interface variables, the actual running code
   isn't immediately known. You see the contract, not the concrete behavior, forcing you to track 
   potential implementations. This obscures logic flow and increases cognitive load compared to 
   concrete types. Overuse can lead to excessive abstraction hiding simple operations. 
2. *Maintainability*: Despite promising decoupling, interfaces create rigid dependency on the contract
   itself. An interface change can trigger cascading required changes across all implementing objects. 
   Debugging is harder because determining the runtime type is needed first to understand the executed
   code path, adding steps to diagnosis.
3. *Code Browse*: Navigation is fragmented. "Go to Definition" on an interface method leads to the
   abstract definition, not running code. Extra steps (like "Find Implementations") are needed to
   find concrete objects, slowing exploration compared to navigating direct calls on concrete types.

### Minimize the Use of interfaces

1. *Guideline*: Avoid introducing interfaces unless there is a clear, compelling need. Always attempt to 
   solve the problem using concrete types first.
2. *Rationale*: While interfaces enable polymorphism and decoupling, they also add a layer of abstraction
   that can sometimes obscure behavior and increase complexity.
3. *Allowed Usage*: Interfaces are appropriate when implementing truly pluggable architectures or defining 
   contracts for components where multiple implementations are expected and necessary (e.g., database adapters).

## Generics (Complexity and Obfuscation)

1. *Readability*: Generics introduce parameterized types, often creating complex, hard-to-read signatures
   with brackets and multiple type parameters (e.g., Copy[M ~map[K]V, K comparable, V any](dst M, src M)).
   These signatures significantly reduce readability compared to straightforward type names.
2. *Maintainability*: Even with Go's relatively simple generics, writing and debugging generic code using
   type parameters/constraints adds complexity over non-generic code. Modifying generic functions/types
   is harder, as changes to constraints or parameters need validation against type rules for all potential
   instantiations. Complex generic interaction errors can still be challenging to resolve. Furthermore,
   inappropriate use can create overly abstract code, making it hard for team members (especially those
   newer to Go generics) to grasp, debug, and modify safely.
3. *Code Browse*: While Go tools help, navigating and understanding generic code demands extra mental
   effort. Developers must constantly track the meaning and limits of type constraints (e.g., [T any],
   [S comparable]) and trace how generic types propagate. This need to reason about abstract type parameters
   and constraints, instead of concrete types, adds a significant layer of complexity to understanding the
   program's logic compared to non-generic code.

### Limit the Use of Generics

1. *Guideline*: Use generics sparingly, prioritizing code readability. Overuse of generics is discouraged.
   If a generic function becomes difficult to understand or reason about, prefer a non-generic implementation.
2. *Rationale*: Generics can significantly increase code complexity and reduce readability, especially when used with
   complex type constraints or nested structures.
3. *Allowed Usage*: Generics are acceptable for simple, well-defined utility or helper functions where they clearly
   reduce boilerplate code without obscuring the function's core purpose (e.g., a function operating on a slice of
   any comparable type). Avoid generics for core business logic or complex data transformations where explicitness
   is preferred.

## Passing Functions as Arguments

1. *Readability*: This pattern shatters linear control flow. Execution jumps from call sites to potentially
   distant/anonymous functions, making the sequence hard to follow and breaking top-to-bottom reading. Deeply
   nested callbacks exemplify the resulting poor readability and reasoning.
2. *Maintainability*: 
    - Unhelpful Error Reports (Stack Traces): Errors inside anonymous functions often yield stack traces
      lacking specific function names. The trace might point to a generic executor, making it much harder
      to quickly pinpoint faulty code compared to errors in named functions. When an error happens inside an
      anonymous function, the error report
    - Complexity of Captured Variables: Anonymous functions capture variables from their creation scope.
      Debugging requires understanding the exact state of these variables when the function runs (potentially
      later). Analyzing this requires care and often causes subtle bugs. Anonymous functions can access and
      use variables from the surrounding code 
    - Difficult Refactoring: When logic is in many anonymous functions passed as arguments, behavior becomes
      distributed and context-dependent. This makes refactoring code significantly harder and riskier than
      modifying logic within a single named function.
3. *Code Browse*: Static analysis and code navigation suffer. "Go to Definition" frequently fails for anonymous
   functions or functions passed as variables, not leading to the runtime implementation. Seeing the next
   executed code becomes harder, forcing more reliance on runtime debugging or mental tracing.

### Restrict Passing Functions as Arguments

1. *Guideline*: Avoid passing functions (callbacks, closures, higher-order functions) as arguments to other functions.
   If logic needed to be pluggable, define an interface and pass concrete implementations (Strategy Pattern).
2. *Rationale*: Passing executable logic as arguments can make control flow harder to follow, increase cognitive 
   load during debugging, and tightly couple the calling context to the function's internal implementation details. 
   We prioritize designs where the sequence of operations is more explicit.
3. *Allowed Usage*: This pattern is strongly discouraged. Before using it, explore alternative designs thoroughly
   (e.g., using concrete types, state machines, or interfaces where appropriate per Guideline 1). Any exceptions 
   must be clearly justified and discussed with the team, demonstrating why simpler alternatives are insufficient.

# Test Coverage and Use of Mocks

When developing software, one of the most crucial aspects is ensuring the functionality of the application
through testing. Let's take a look at the concept of test coverage and the role of mocks in testing.

## Test Coverage

Test coverage is a measure of the amount of testing performed by a set of tests. It includes various forms
of coverage like function coverage, statement coverage, branch coverage, etc. High test coverage is desirable
as it's an indicator of how much of the code is tested, reducing the chance of an undetected bug making
its way into production.

However, 100% test coverage doesn't guarantee that there are no bugs in the code. It just ensures that
every line of code is executed at least once in the tests. It doesn't cover different combinations of
inputs or paths through the code. Thus, while striving for high test coverage, also focus on the quality
of the tests and the meaningful scenarios they cover.

## Use of Mocks

Mocks are objects that simulate the behavior of real objects. They are used when the real objects are
impractical or impossible to incorporate into the unit test. Using mocks in testing has both advantages
and disadvantages:

**Advantages**

1. **Isolation**: Mocks can isolate the code under test, ensuring the test only fails when there's a problem
   with the code itself and not with its dependencies.
2. **Simplicity**: They can simplify the setup for a test by replacing a dependency that's complex to set up
   with a mock.
3. **Control**: Mocks allow you to control the test environment, making it possible to test scenarios that
   might be hard to test with real objects.

**Disadvantages**

1. **Over-specification**: Tests with mocks can become tightly coupled to the implementation details of the
   code under test, meaning they may fail if the implementation changes but the behavior doesn't.
2. **Maintenance**: Mocks require maintenance as the code evolves. If the interface of the real object changes,
   all the mocks of that object need to be updated.
3. **False Positives/Negatives**: Mocks can lead to false positives if they don't behave exactly like the real
   objects they're mocking. They can also hide errors if the mock is not an accurate representation of
   the dependency.

Due to these disadvantages, it's advised to use mocks sparingly, only when absolutely necessary.
Instead, aim for designing code that's easy to test without the need for mocks, by using techniques
such as dependency injection, clear separation of concerns, and designing for testability from the outset.
This will help keep your tests robust and maintainable over time.

# Performance Considerations

Efficiency and performance are crucial factors when developing a project. In Go, the way we implement code can
significantly impact the execution speed and memory usage of our programs. Here are some guiding principles for
ensuring we prioritize performance:

## Making Informed Choices

There is often more than one way to implement core functionality. Each approach may have different performance
characteristics due to factors like algorithmic complexity, memory usage, concurrency, and more.

Before deciding, we should consider the known performance implications of each option. Understand the nature
of the task, the data involved, and our application's specific circumstances. Then, choose the option that
seems most likely to provide the best performance.

Keep in mind that while performance is important, we must also consider code readability, maintainability,
and the cost of development time. Sometimes, the most performant solution might not be the most suitable
one if it overly complicates the code or makes it harder to maintain.

## The Role of Benchmarking

Benchmarking involves writing specific tests that measure our code's performance under different conditions.
In Go, the built-in testing package provides a benchmarking feature for this.

Benchmarking helps us gather hard data about our application's performance, identify bottlenecks,
and verify if certain parts of the application perform as expected. If results show that a particular
piece of code performs slower than anticipated, we can explore alternative approaches.

However, it's crucial to remember that benchmarking tests in Go might not capture performance issues
that only manifest under scale or in production-like environments. Benchmarks run in isolation and
typically under controlled and limited conditions. They might not take into account factors like
network latency, disk I/O, hardware limitations, or the interaction between different parts of the
system under heavy load.

Therefore, in addition to regular benchmarking, it's also crucial to perform load testing and profiling
in a production-like environment. This will give us a more realistic understanding of how our application
performs under real-world conditions and can help identify performance bottlenecks that we might not
catch through benchmarking alone.

In the face of performance discussions or debates, it's important to use data to guide decisions about
performance optimizations. Use the insights from benchmarking and load testing to guide the conversation
and decision-making process. This way, we maintain a data-driven approach to performance optimization.

By following these guidelines, we help ensure our application is performant while maintaining a productive
approach to performance optimization.

## Post-Benchmarking and Load Testing Steps

Once you've performed benchmarking and load testing, and identified potential performance bottlenecks,
it's important to follow a structured approach to address these issues:

1. **Analyse the Data**: Review the data and results from your benchmarking and load testing efforts.
   Identify the areas of your code that are potential bottlenecks. Try to understand why these areas are
   underperforming - is it due to high I/O operations, inefficient algorithms, lack of concurrency, or
   some other reason?
2. **Discuss with the Code Owner**: Once you've identified a performance issue, bring it up with the
   code owner or the person responsible for that area of the code. It's important to approach this in a
   constructive way, focusing on the issue at hand and not the person. Present your findings, explain why
   you believe there's a problem, and discuss your thoughts on potential ways to improve the performance.
3. **Explore Solutions**: Discuss and brainstorm potential solutions to the identified performance issue.
   There might be different ways to address the problem, each with its own trade-offs. Possible solutions
   might involve optimizing the existing code, using a different algorithm or data structure, introducing
   concurrency, or even re-architecting a part of the system.
4. **Decide on a Solution**: After discussing the available options, decide on a solution together with
   the code owner and other relevant stakeholders. This decision should be based on the potential
   performance gains, the trade-offs involved, the amount of work required to implement the solution,
   and the overall impact on the codebase and system architecture.
5. **Implement, Test and Review**: Once a decision has been made, implement the solution. After the changes
   have been made, conduct a code review to ensure the solution has been implemented correctly and hasn't
   introduced new issues. It's also crucial to repeat the benchmarking and load testing process to validate
   that the changes have indeed improved the performance and that no new bottlenecks have been introduced.

Following this approach, we can systematically and effectively address performance bottlenecks, leading
to a more robust and efficient application. It's a continuous cycle - as the application evolves,
new bottlenecks might emerge, and the process starts again.

# Code Owners

In the context of software development, code ownership assigns the responsibility of specific parts of the
codebase to certain individuals or teams, these being the code owners. This concept is widely utilized in
both small teams and large-scale open-source projects to maintain code quality, foster accountability, and
streamline the code review process.

## GitHub's CODEOWNERS Feature

Recognizing the importance of code ownership, GitHub introduced the CODEOWNERS feature. This feature allows
repository administrators to define which individuals or teams own specific files or directories in a repository.

When changes to owned files are proposed (via pull requests), GitHub automatically requests a review from the
appropriate code owners. This automation streamlines the review process and ensures that the right
individuals are included in the review process.

The CODEOWNERS feature enforces the principle of code ownership, enhancing code quality and accountability
within projects hosted on GitHub. It's a powerful tool for teams of all sizes, ensuring that all changes
are reviewed by individuals with the most context and understanding of the code being changed. This can lead
to better code, more effective reviews, and a more successful project overall.

## Who Can Be Code Owners?

Code owners can be individuals or teams who have shown a deep understanding and expertise of certain parts
of the codebase. They can be:

1. **Original Authors**: Often, the individual who originally wrote a particular component or package is
   best suited to own it, given their intimate knowledge of its design, functionality, and purpose.
2. **Senior Developers**: Senior or experienced developers who have a thorough knowledge and understanding
   of the codebase are typically good candidates for code ownership.
3. **Software Architects**: Architects, especially those who have designed certain aspects of the system,
   can also be code owners due to their high-level understanding of how the code fits into the overall architecture.

## Responsibilities of Code Owners

The specific responsibilities of code owners can vary but generally include the following:

1. **Maintaining Code Quality**: Code owners are primarily responsible for ensuring the quality of their code.
   This includes creating clean, efficient, and maintainable code, and adhering to the project's coding
   standards and guidelines.
2. **Reviewing Changes**: Code owners are responsible for reviewing proposed changes to their sections of the
   codebase. They must ensure these changes adhere to established coding standards, don't introduce new issues,
   and align with the project's overall architecture and design.
3. **Documentation**: It's typically the code owner's responsibility to document their sections of the code.
   This can include inline comments, README files, and formal technical documentation.
4. **Handling Bugs and Issues**: When bugs or issues arise in their areas of the codebase, code owners are
   often the first line of response. They are expected to either handle these issues themselves or guide others
   in resolving them.
5. **Knowledge Sharing**: Given their expertise, code owners are often expected to share their knowledge with
   the rest of the team. This can involve explaining their code to others, assisting new team members in getting
   up to speed, and promoting best practices.

By fulfilling these responsibilities, code owners help to ensure the stability, quality, and maintainability of the software
projects they contribute to.

# Pre-Pull Request Checklist

Before you submit a Pull Request, it's crucial to perform a thorough check of your own work. This Pre-Pull Request
Checklist serves as a guide to help you ensure your code is ready for review by others:

1. **Code Accuracy**: Verify that your code works as intended, covering not only primary functionality but also
edge cases and error handling.
2. **Clarity**: Ensure your code is readable and understandable. Use clear naming conventions, maintain a clean
structure, and include concise, relevant comments.
3. **Consistency**: Confirm your code adheres to the conventions and patterns present in the rest of the codebase.
This promotes readability and maintainability.
4. **Optimization**: Reflect on whether your code is as efficient as possible. If there are areas where you can
improve performance without sacrificing clarity, it's worth considering.
5. **Testing**: Make sure your code is well-tested. Tests should cover all key functionalities and edge cases.
6. **Documentation/Code Comments**: Check that your code is adequately documented. Good documentation helps others
understand the purpose and functionality of your code. If there is a section of code that is temporary, or
if there are specific reasons for a particular implementation, please articulate them using comments.

The purpose of Pre-Pull Request Checklist is not to prove perfection, but to catch and correct issues early.
This not only helps accelerate the review process, but it also fosters a more efficient, and collaborative
working environment.
