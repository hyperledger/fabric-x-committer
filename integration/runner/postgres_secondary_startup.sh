#!/usr/bin/env bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

# Bootstrap a PostgreSQL streaming replication standby.
# This script is embedded in Go via go:embed and invoked with:
#   bash -c "$script" bash <primary_host>
# where $1 is the primary node's container IP.

set -eu

primary_host="$1"

# Wait for the primary to accept connections.
until pg_isready -h "$primary_host" -U yugabyte; do sleep 1; done

# Prepare an empty PGDATA owned by the postgres OS user.
mkdir -p /var/lib/postgresql/data
chown postgres:postgres /var/lib/postgresql/data
chmod 700 /var/lib/postgresql/data

# Clone the primary's data directory.
# -R auto-creates standby.signal and primary_conninfo so postgres starts in standby mode.
# gosu is pre-installed in the official image — needed because the container
# runs as root but postgres processes must run as the "postgres" OS user.
PGPASSWORD=repl_password gosu postgres \
  pg_basebackup -h "$primary_host" -U repl_user -D /var/lib/postgresql/data -Fp -Xs -R

# Start the standby. exec replaces the shell so postgres receives Docker signals directly.
exec gosu postgres postgres -D /var/lib/postgresql/data -c hot_standby=on
