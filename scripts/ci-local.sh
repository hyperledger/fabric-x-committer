#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

# Runs the full CI test flow locally. Manages DB lifecycle automatically:
# starts postgres/yugabyte before tests that need them, stops them after.
#
# Usage: make ci-local
#
# DB resiliency and container tests auto-provision their own containers.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_DIR"

RED='\033[0;31m'
GREEN='\033[0;32m'
BOLD='\033[1m'
RESET='\033[0m'

step() {
    echo -e "\n${BOLD}=== $1 ===${RESET}\n"
}

cleanup() {
    step "Cleanup"
    stop_db postgres
    stop_db yugabyte
    make kill-test-docker
}
trap cleanup EXIT

# --- Database lifecycle ---
# Lookup helpers for DB config. Using functions instead of associative arrays
# for bash 3.2 compatibility (macOS default).

db_container() {
    case "$1" in
        postgres) echo "sc_test_postgres_unit_tests" ;;
        yugabyte) echo "sc_test_yugabyte" ;;
    esac
}

db_script() {
    case "$1" in
        postgres) echo "get-and-start-postgres.sh" ;;
        yugabyte) echo "get-and-start-yuga.sh" ;;
    esac
}

db_timeout() {
    case "$1" in
        postgres) echo 30 ;;
        yugabyte) echo 120 ;;
    esac
}

# pg_isready path differs per image: postgres has it on PATH,
# yugabyte has it at /home/yugabyte/postgres/bin/pg_isready.
db_pg_isready() {
    case "$1" in
        postgres) echo "pg_isready" ;;
        yugabyte) echo "/home/yugabyte/postgres/bin/pg_isready" ;;
    esac
}

db_running() {
    docker ps -q -f "name=$(db_container "$1")" 2>/dev/null | grep -q .
}

start_db() {
    local db="$1"
    if db_running "$db"; then
        echo "$db already running"
        return
    fi
    step "Starting $db"
    bash "$SCRIPT_DIR/$(db_script "$db")"
    echo "Waiting for $db to be ready..."
    for i in $(seq 1 "$(db_timeout "$db")"); do
        if docker exec "$(db_container "$db")" "$(db_pg_isready "$db")" -h 127.0.0.1 >/dev/null 2>&1; then
            echo "$db is ready"
            return
        fi
        sleep 1
    done
    echo -e "${RED}$db failed to start${RESET}" >&2
    exit 1
}

stop_db() {
    local db="$1"
    if db_running "$db"; then
        echo "Stopping $db"
        docker rm -f "$(db_container "$db")" >/dev/null 2>&1 || true
    fi
}

# --- Test steps ---

run_step() {
    local name="$1"
    shift
    step "$name"
    "$@"
    echo -e "${GREEN}PASSED: $name${RESET}"
}

# set -e ensures any failure triggers the EXIT trap for cleanup, then exits.

# 0. Clean slate
make kill-test-docker

# 1. Code generation, lint, and build
run_step "Generate (proto + mocks)" make proto mocks
run_step "Lint" make lint
run_step "Build" make build

# 2. Unit tests (no DB)
run_step "Unit Tests (no DB)" make test-no-db

# 3. DB tests with Postgres
start_db postgres
run_step "DB Tests (Postgres)" env DB_DEPLOYMENT=local DB_TYPE=postgres make test-all-db MAKEFLAGS=--jobs=4
stop_db postgres

# 4. Core DB tests + integration tests with YugabyteDB
start_db yugabyte
# Reduce parallelism for YugabyteDB tests. YugabyteDB's catalog is a single-tablet
# table, so concurrent DDL from many parallel test processes causes serialization
# conflicts. CI avoids this by running separate jobs on fresh instances.
run_step "Core DB Tests (YugabyteDB)" env DB_DEPLOYMENT=local make test-core-db MAKEFLAGS=--jobs=4
run_step "Integration Tests (YugabyteDB)" env DB_DEPLOYMENT=local make test-integration MAKEFLAGS=--jobs=4
stop_db yugabyte

# 5. DB resiliency tests (auto-provisions containers)
run_step "DB Resiliency Tests" make test-integration-db-resiliency

# 6. Container tests
run_step "Container Tests" make test-container

make kill-test-docker

# Disable the EXIT trap — cleanup is already done at this point.
# Without this, the trap's stop_yugabyte sends SIGTERM which propagates to Make.
trap - EXIT
echo -e "\n${GREEN}${BOLD}All CI steps executed successfully.${RESET}\n"
