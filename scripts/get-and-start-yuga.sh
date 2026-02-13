#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

YUGA_DIR=${YUGA_DIR:="$HOME/bin/yugabyte"}

if [ ! -d "$YUGA_DIR" ]; then
  echo "Downloading and extracting YugabyteDB"
  mkdir -p "$YUGA_DIR"
  curl https://downloads.yugabyte.com/releases/2025.2.0.1/yugabyte-2025.2.0.1-b1-linux-x86_64.tar.gz |
    tar --strip=1 -xz -C "$YUGA_DIR"

  cd "$YUGA_DIR" || exit 1

  echo "Installing YugabyteDB"
  bin/post_install.sh
fi

cd "$YUGA_DIR" || exit 1

echo "Unlimited open files and enabling transparent huge pages..."
ulimit -n 100000
echo always | sudo tee /sys/kernel/mm/transparent_hugepage/enabled
echo always | sudo tee /sys/kernel/mm/transparent_hugepage/defrag

DATA_DIR=$(mktemp -d -t "yuga.XXXX")
echo "Using temporary data dir: $DATA_DIR"

echo "Running YugabyteDB"
alias python='${PYTHON_CMD:-python}'
${PYTHON_CMD:-python} -m pip install setuptools
bin/yugabyted start \
  --advertise_address 0.0.0.0 \
  --callhome false \
  --fault_tolerance none \
  --background true \
  --ui false \
  --base_dir "$DATA_DIR" \
  --insecure \
  --tserver_flags "ysql_max_connections=500,tablet_replicas_per_gib_limit=4000,yb_num_shards_per_tserver=1,minloglevel=3,yb_enable_read_committed_isolation=true"
#  By default, 1 GB of memory reserved for a YB-Tserver can support up to 1497
#  tablets. When tests are run in parallel, this limit is sometimes reached in
#  environments with low resource allocation, causing the test to fail. To handle
#  such cases, we are increasing the limit to 4000. Note that this increase is not
#  recommended for production and is intended solely for running the test.
