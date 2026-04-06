#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

YUGA_IMAGE="yugabytedb/yugabyte:2025.2.0.1-b1"
YUGA_CONTAINER="sc_test_yugabyte"

echo "Downloading YugabyteDB"
docker pull "$YUGA_IMAGE"

echo "Running YugabyteDB container"
docker run --name "$YUGA_CONTAINER" \
  -p 5433:5433 \
  -d "$YUGA_IMAGE" \
  bin/yugabyted start \
    --advertise_address 0.0.0.0 \
    --callhome false \
    --fault_tolerance none \
    --background false \
    --ui false \
    --insecure \
    --tserver_flags "pgsql_proxy_bind_address=0.0.0.0,ysql_max_connections=500,tablet_replicas_per_gib_limit=4000,yb_num_shards_per_tserver=1,minloglevel=3,yb_enable_read_committed_isolation=true"
#  By default, 1 GB of memory reserved for a YB-Tserver can support up to 1497
#  tablets. When tests are run in parallel, this limit is sometimes reached in
#  environments with low resource allocation, causing the test to fail. To handle
#  such cases, we are increasing the limit to 4000. Note that this increase is not
#  recommended for production and is intended solely for running the test.
