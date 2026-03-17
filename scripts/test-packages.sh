#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

# Test a list of packages.
#
# Usage:
# scripts/test-packages.sh "PKG1 [[PKG2] ...]" [ARGS ...]
# The first argument is a space separated list of packages.
# The following arguments will be passed along.
#
# Uses "gotestsum" to summerize the output.
# * "--rerun-fails=0" is used to re-run failed test (once).
#   The re-run serves two purposes.
#     1. Conveniently show the failed test at the end along with their output, to allow easy debugging.
#     2. Mitigate flaky tests.
# * "--format dots" is used to print a "." for passed test, and "x" for a failed test.
#   This reduces the output clutter.
#   Failed tests output will be showed at the end when they re-run.
#   Successful tests' output is not shown.
# * "--packages ${packages}" is required to support "--rerun-fails=0".

packages="$1"
shift

echo "Running test for packages: ${packages}"
gotestsum --rerun-fails=0 --format dots --packages "${packages}" -- -v -timeout 30m "$@"
