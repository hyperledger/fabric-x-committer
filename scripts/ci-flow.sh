#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

export DB_DEPLOYMENT=local

echo "Unit Test (non DB)" | tee /tmp/sc-tests.txt
make test-no-db "$@" &>>/tmp/sc-tests.txt

echo >>/tmp/sc-tests.txt
echo "Unit Tests (with DB)" | tee -a /tmp/sc-tests.txt
make test-requires-db test-core-db "$@" &>>/tmp/sc-tests.txt

echo >>/tmp/sc-tests.txt
echo "Integration Tests" | tee -a /tmp/sc-tests.txt
make test-integration test-intergration-runtime "$@" &>>/tmp/sc-tests.txt

echo >>/tmp/sc-tests.txt
echo "Build and test all-in-one test image (container)" | tee -a /tmp/sc-tests.txt
make test-container "$@" &>>/tmp/sc-tests.txt

echo >>/tmp/sc-tests.txt
echo "Searching fails" | tee -a /tmp/sc-tests.txt
grep FAIL <tmp/sc-tests.txt | tee -a /tmp/sc-tests.txt
