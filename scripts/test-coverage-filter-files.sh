#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

# We filter some of the files for test coverage reporting.
# The "mocks/" folder is reported for two reasons:
# 1. We provide a mock CLI as part of our published deliverables.
# 2. It contain non-trivial testing code with high complexity.
sed -i -E -f - coverage.profile <<EOF
# The main file cannot be covered by tests as it may call os.Exit(1).
/main\.go/d
# Generated files (e.g., mocks, protobuf) may contain unused methods.
/\.pb(\.gw)?\.go/d
/\.mock\.go/d
# Test files that are included in non-test files.
/test_exports?\.go/d
/utils\/test/d
/service\/vc\/dbtest/d
EOF
