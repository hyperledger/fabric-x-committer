#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

echo "Downloading Postgres"
docker pull postgres:16.9-alpine3.21

echo "Running Postgres container"
docker run --name sc_test_postgres_unit_tests \
  -e POSTGRES_PASSWORD=yugabyte \
  -e POSTGRES_USER=yugabyte \
  -p 5433:5432 \
  -d postgres:16.9-alpine3.21
