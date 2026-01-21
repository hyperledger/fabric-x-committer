# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#
#########################
# Makefile Targets Summary
#########################
#
# Tests:
#   test                         - Run unit tests (excludes integration and container tests)
#   test-package-%               - Run tests for a specific package
#   test-integration             - Run integration tests (excludes DB resiliency tests)
#   test-integration-db-resiliency - Run DB resiliency integration tests
#   test-container               - Run container tests
#   test-core-db                 - Run tests for components that directly talk to the DB
#   test-requires-db             - Run tests for components that depend on DB layer
#   test-no-db                   - Run tests that require no DB
#   test-fuzz                    - Run ASN.1 marshalling fuzz tests
#   test-cover                   - Run tests with coverage
#   test-cover-%                 - Run tests with coverage for a specific package
#   cover-report                 - Generate HTML coverage report
#   cover-report-%               - Generate HTML coverage report for a specific package
#
# Build:
#   build                        - Build all binaries
#   build-arch                   - Build binaries for linux/(current-arch amd64 arm64 s390x)
#   build-arch-%                 - Build binaries for a specific os-arch (e.g., build-arch-linux-amd64)
#   build-cli-%                  - Build a specific CLI binary
#   build-docker                 - Build all binaries in a docker container
#   build-test-node-image        - Build the test node docker image
#   build-release-image          - Build the release docker image
#   build-test-genesis-block     - Build the test genesis block
#
# Benchmarks:
#   bench-loadgen                - Run load generation benchmarks
#   bench-dep                    - Run dependency detector benchmarks
#   bench-preparer               - Run preparer benchmarks
#   bench-sign                   - Run signature benchmarks
#   bench-sidecar                - Run sidecar benchmarks
#
# Linting:
#   lint                         - Run all linters (Go, SQL, proto, license, metrics doc)
#   lint-proto                   - Run protobuf linters
#   full-lint                    - Run Go linter on all packages
#   full-lint-%                  - Run Go linter on a specific package
#
# Code Generation:
#   proto                        - Generate protobuf code
#   generate-metrics-doc         - Generate metrics reference documentation
#
# Documentation:
#   check-metrics-doc            - Check if metrics documentation is up to date
#
# Cleanup:
#   clean                        - Remove all binaries
#   kill-test-docker             - Kill test docker containers

#########################
# Constants
#########################

go_cmd          ?= go
version         := latest
project_dir     := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
output_dir      ?= $(project_dir)/bin
arch_output_dir ?= $(project_dir)/archbin
cache_dir       ?= $(shell $(go_cmd) env GOCACHE)
mod_cache_dir   ?= $(shell $(go_cmd) env GOMODCACHE)
go_version      ?= 1.24.3
golang_image    ?= golang:$(go_version)-bookworm

dockerfile_base_dir       ?= $(project_dir)/docker/images
dockerfile_test_node_dir  ?= $(dockerfile_base_dir)/test_node
dockerfile_release_dir    ?= $(dockerfile_base_dir)/release

# Set this parameter when running docker-builder-run
# E.g., make docker-builder-run cmd="make build-local"
cmd            ?=

# Set the 'docker_cmd' variable directly (e.g., docker_cmd="podman") to override automatic detection.
# If the 'docker_cmd' variable is not set, the script will find and use either Docker or Podman.
# An error will occur if neither container engine is installed.
docker_cmd ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || \
							echo podman || { echo "Error: Neither Docker nor Podman is installed." >&2; exit 1; })
image_namespace=docker.io/hyperledger

# Set these parameters to compile to a specific os/arch
# E.g., make build-local os=linux arch=amd64
os             ?= $(shell $(go_cmd) env GOOS)
arch           ?= $(shell $(go_cmd) env GOARCH)
multiplatform  ?= false
env            ?= env GOOS=$(os) GOARCH=$(arch)
build_flags    ?= -buildvcs=false -o
go_build       ?= $(env) $(go_cmd) build $(build_flags)
go_test        ?= $(env) $(go_cmd) test -json -v -timeout 30m
proto_flags    ?=

ifneq ("$(wildcard /usr/include)","")
    proto_flags += --proto_path="/usr/include"
endif

# Homebrew paths (Apple Silicon and Intel)
ifneq ("$(wildcard /opt/homebrew/include)","")
    proto_flags += --proto_path="/opt/homebrew/include"
endif

arch_output_dir_rel = $(arch_output_dir:${project_dir}/%=%)

PYTHON_CMD ?= $(shell command -v python3 2>/dev/null || command -v python 2>/dev/null)

# Set additional parameter to build the test-node for different platforms and push
# E.g., make multiplatform=true docker_push=true build-test-node-image
docker_build_flags=--quiet
ifeq "$(multiplatform)" "true"
	docker_build_flags+=--platform linux/amd64,linux/arm64
endif

docker_push ?= false
docker_push_arg = 
ifeq "$(docker_push)" "true"
	docker_push_arg=--push
endif

MAKEFLAGS += --jobs=16

#########################
# Tests
#########################

ROOT_PKG_REGEXP = github.com/hyperledger/fabric-x-committer
CORE_DB_PACKAGES_REGEXP = ${ROOT_PKG_REGEXP}/service/(vc|query)
REQUIRES_DB_PACKAGES_REGEXP = ${ROOT_PKG_REGEXP}/(service/coordinator|loadgen|cmd)
HEAVY_PACKAGES_REGEXP = ${ROOT_PKG_REGEXP}/(docker|integration)

NON_HEAVY_PACKAGES=$(shell $(go_cmd) list ./... | grep -vE "$(HEAVY_PACKAGES_REGEXP)")
COR_DB_PACKAGES=$(shell $(go_cmd) list ./... | grep -E "$(CORE_DB_PACKAGES_REGEXP)")
REQUIRES_DB_PACKAGES=$(shell $(go_cmd) list ./... | grep -E "$(REQUIRES_DB_PACKAGES_REGEXP)")
NO_DB_PACKAGES=$(shell $(go_cmd) list ./... | grep -vE "$(CORE_DB_PACKAGES_REGEXP)|$(REQUIRES_DB_PACKAGES_REGEXP)|$(HEAVY_PACKAGES_REGEXP)")

GO_TEST_FMT_FLAGS := -hide empty-packages


# Excludes integration and container tests.
# Use `test-integration`, `test-integration-db-resiliency`, and `test-container`.
test: build
	@$(go_test) ${NON_HEAVY_PACKAGES} | gotestfmt ${GO_TEST_FMT_FLAGS}

# Test a specific package.
test-package-%: build
	@$(go_test) ./$*/... | gotestfmt ${GO_TEST_FMT_FLAGS}

# Integration tests excluding DB resiliency tests.
# Use `test-integration-db-resiliency`.
test-integration: build
	@$(go_test) ./integration/... -skip "DBResiliency.*" | gotestfmt ${GO_TEST_FMT_FLAGS}

# DB resiliency integration tests.
test-integration-db-resiliency: build
	@$(go_test) ./integration/... -run "DBResiliency.*" | gotestfmt ${GO_TEST_FMT_FLAGS}

# Tests the all-in-one docker image.
test-container: build-test-node-image build-release-image
	@$(go_test) ./docker/... | gotestfmt ${GO_TEST_FMT_FLAGS}

# Tests for components that directly talk to the DB, where different DBs might affect behaviour.
test-core-db: build
	@$(go_test)  ${COR_DB_PACKAGES} | gotestfmt ${GO_TEST_FMT_FLAGS}

# Tests for components that depend on the DB layer, but are agnostic to the specific DB used.
test-requires-db: build
	@$(go_test) ${REQUIRES_DB_PACKAGES} | gotestfmt ${GO_TEST_FMT_FLAGS}

# Tests the ASN.1 marshalling using fuzz testing.
test-fuzz: build
	@$(go_test) -run="^$$" -fuzz=".*" -fuzztime=5m ./utils/signature | gotestfmt ${GO_TEST_FMT_FLAGS}

# Tests that require no DB at all, e.g., pure logic, utilities
test-no-db: build
	@$(go_test) ${NO_DB_PACKAGES} | gotestfmt ${GO_TEST_FMT_FLAGS}

test-cover: build
	$(go_cmd) test -v -coverprofile=coverage.profile ./...

test-cover-%: build
	$(go_cmd) test -v -coverprofile=$*.coverage.profile "./$*/..."

cover-report: FORCE
	$(go_cmd) tool cover -html=coverage.profile

cover-report-%: FORCE
	$(go_cmd) tool cover -html=$*.coverage.profile

clean: FORCE
	@rm -rf $(output_dir)
	@rm -rf $(arch_output_dir)

kill-test-docker: FORCE
	$(docker_cmd) ps -aq -f "name=sc_test" | xargs $(docker_cmd) rm -f

#########################
# Benchmarks
#########################

# Run a load generation benchmarks with added TX/sec column.
bench-loadgen: FORCE
	$(go_cmd) test ./loadgen/... -bench "Benchmark.*" -run="^$$" | awk -f scripts/bench-tx-per-sec.awk

# Run dependency detector benchmarks with added op/sec column.
bench-dep: FORCE
	$(go_cmd) test ./service/coordinator/dependencygraph/... -timeout 60m -bench "BenchmarkDependencyGraph.*" -run="^$$" | awk -f scripts/bench-tx-per-sec.awk

# Run dependency detector benchmarks with added op/sec column.
bench-preparer: FORCE
	$(go_cmd) test ./service/vc/... -bench "BenchmarkPrepare.*" -run "^$$" | awk -f scripts/bench-tx-per-sec.awk

# Run signature benchmarks with added op/sec column.
bench-sign: FORCE
	$(go_cmd) test ./utils/signature/... -bench ".*" -run "^$$" | awk -f scripts/bench-tx-per-sec.awk

# Run sidecar benchmarks with added op/sec column.
bench-sidecar: FORCE
	$(go_cmd) test ./service/sidecar/... -bench "Benchmark.*" -run "^$$" | awk -f scripts/bench-tx-per-sec.awk

#########################
# Generate protos
#########################

PROTO_COMMON_DIR="$(shell $(env) $(go_cmd) list -m -f '{{.Dir}}' github.com/hyperledger/fabric-x-common)"

BUILD_DIR := .build
PROTOS_API_REPO := https://github.com/googleapis/googleapis.git
PROTOS_API_DIR := ${BUILD_DIR}/googleapis
# We depend on this specific file to ensure the repo is actually cloned
PROTOS_SENTINEL := ${PROTOS_API_DIR}/.git

proto: FORCE $(PROTOS_SENTINEL)
	@echo "Generating protobufs: $(shell find ${project_dir}/api -name '*.proto' -print0 \
		| xargs -0 -n 1 dirname | xargs -n 1 basename | sort -u)"
	@protoc \
	  --go_out=paths=source_relative:. \
	  --go-grpc_out=. \
	  --go-grpc_opt=paths=source_relative \
	  --grpc-gateway_out=. \
	  --grpc-gateway_opt=paths=source_relative \
	  --proto_path="${project_dir}" \
	  --proto_path="${PROTO_COMMON_DIR}" \
	  --proto_path="${PROTOS_API_DIR}" \
	  ${proto_flags} \
	  ${project_dir}/api/*/*.proto

lint-proto: FORCE $(PROTOS_SENTINEL)
	@echo "Running protobuf linters..."
	@api-linter \
		-I="${project_dir}/api" \
		-I="${PROTO_COMMON_DIR}" \
		-I="${PROTOS_API_DIR}" \
		--config .apilinter.yaml \
		--set-exit-status \
		--output-format github \
		$(shell find ${project_dir}/api -name '*.proto' | sed 's|${project_dir}/api/||')

$(PROTOS_SENTINEL):
	@echo "Cloning googleapis..."
	@mkdir -p ${BUILD_DIR}
	@rm -rf ${PROTOS_API_DIR} # Ensure we start fresh if re-cloning
	@git -c advice.detachedHead=false clone --single-branch --depth 1 ${PROTOS_API_REPO} ${PROTOS_API_DIR}

#########################
# Binaries
#########################

$(output_dir):
	mkdir -p "$(output_dir)"

$(cache_dir) $(mod_cache_dir):
	# Use the host local gocache and gomodcache folder to avoid rebuilding and re-downloading every time
	mkdir -p "$(cache_dir)" "$(mod_cache_dir)"

BUILD_TARGETS=build-cli-committer build-cli-loadgen build-cli-mock

build: $(output_dir) $(BUILD_TARGETS)

build-arch: build-arch-linux-$(arch) build-arch-linux-amd64 build-arch-linux-arm64 build-arch-linux-s390x

build-arch-%: FORCE
	@CGO_ENABLED=0 make \
		os=$(word 1, $(subst -, ,$*)) \
		arch=$(word 2, $(subst -, ,$*)) \
		output_dir=$(arch_output_dir)/$* \
		build_flags="-ldflags '-w -s' $(build_flags)" \
		build

build-cli-%: FORCE $(output_dir)
	$(go_build) "$(output_dir)/$*" "./cmd/$*"

build-docker: FORCE $(cache_dir) $(mod_cache_dir)
	$(docker_cmd) run --rm -it \
	  --mount "type=bind,source=$(project_dir),target=$(project_dir)" \
	  --mount "type=bind,source=$(cache_dir),target=$(cache_dir)" \
	  --mount "type=bind,source=$(mod_cache_dir),target=$(mod_cache_dir)" \
	  --workdir $(project_dir) \
	  --env GOCACHE="$(cache_dir)" \
	  --env GOMODCACHE="$(mod_cache_dir)" \
	  $(golang_image) \
    make build output_dir=$(output_dir) env="$(env)"
	scripts/amend-permissions.sh "$(cache_dir)" "$(mod_cache_dir)"

build-test-node-image: build-arch build-test-genesis-block
	${docker_cmd} build $(docker_build_flags) \
		-f $(dockerfile_test_node_dir)/Dockerfile \
		-t ${image_namespace}/committer-test-node:${version} \
		--build-arg ARCHBIN_PATH=${arch_output_dir_rel} \
		. $(docker_push_arg)

build-release-image: build-arch
	./scripts/build-release-image.sh \
		$(docker_cmd) $(version) $(image_namespace) $(dockerfile_release_dir) $(multiplatform) $(arch_output_dir_rel)

build-test-genesis-block: $(output_dir) build-cli-loadgen
	@# We load the env from the Dockerfile to use them to generate the config block.
	env -v $(shell grep '^ENV' $(dockerfile_test_node_dir)/Dockerfile | cut -d' ' -f2- | xargs) \
		bin/loadgen make-genesis-block \
		-c "$(project_dir)/cmd/config/samples/loadgen.yaml" \
		>"$(output_dir)/sc-genesis-block.proto.bin"

#########################
# Linter
#########################

lint: check-metrics-doc lint-proto FORCE
	@echo "Running Go Linters..."
	golangci-lint run --color=always --new-from-rev=main --timeout=4m
	@echo "Running SQL Linters..."
	git ls-files '*.sql' | sort -u | ${PYTHON_CMD} -m sqlfluff lint --dialect postgres
	@echo "Running License Header Linters..."
	scripts/license-lint.sh

# This rule can be used to find and fix lint issues for specific package.
full-lint-%: FORCE
	golangci-lint run --color=always --timeout=4m ./$*/...

full-lint: FORCE
	golangci-lint run --color=always --timeout=4m ./...

generate-metrics-doc: FORCE
	scripts/metrics_doc.sh generate

check-metrics-doc: FORCE
	scripts/metrics_doc.sh check

# https://www.gnu.org/software/make/manual/html_node/Force-Targets.html
# If a rule has no prerequisites or recipe, and the target of the rule is a nonexistent file,
# then make imagines this target to have been updated whenever its rule is run.
# This implies that all targets depending on this one will always have their recipe run.
FORCE:
