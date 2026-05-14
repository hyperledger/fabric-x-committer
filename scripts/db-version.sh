#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

until psql -c "SELECT 1" > /dev/null 2>&1; do
  echo "DB is unavailable; waiting 1 second"
  sleep 1
done
psql -c "SELECT version();"
