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
#   test-all-db                  - Run core-db and required-db tests
#   test-fuzz                    - Run ASN.1 marshalling fuzz tests
#   test-cover                   - Run tests with coverage
#   test-cover-%                 - Run tests with coverage for a specific package
#   cover-report                 - Generate HTML coverage report
#   cover-report-%               - Generate HTML coverage report for a specific package
#
# Build:
#   build                        - Build all binaries
#   build-release                - Build release binaries for linux/(current-arch amd64 arm64 s390x)
#   build-release                - Build release binaries for a specific os-arch (e.g., build-arch-linux-amd64)
#   build-cmd-%                  - Build a specific CMD binary
#   build-image-%                - Build a docker image (test-node or release)
#   build-with-docker            - Build all binaries in a docker container
#
# Benchmarks:
#   bench-loadgen                - Run load generation benchmarks
#   bench-dep                    - Run dependency detector benchmarks
#   bench-preparer               - Run preparer benchmarks
#   bench-sign                   - Run signature benchmarks
#   bench-sidecar                - Run sidecar benchmarks
#   bench-deliver                - Run delivery benchmarks
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
#   mocks                        - Generate mocks
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

go_cmd              ?= go
version             := latest
project_path        := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
bin_dir             ?= bin
release_dir         ?= release
bin_path            ?= $(project_path)/$(bin_dir)
release_path        ?= $(project_path)/$(release_dir)
cache_path          ?= $(shell $(go_cmd) env GOCACHE)
mod_cache_path      ?= $(shell $(go_cmd) env GOMODCACHE)
go_version          ?= $(shell $(go_cmd) list -m -f '{{.GoVersion}}')
golang_image        ?= golang:$(go_version)-bookworm

dockerfile_base_path       ?= $(project_path)/docker/images
dockerfile_test_node_path  ?= $(dockerfile_base_path)/test_node
dockerfile_release_path    ?= $(dockerfile_base_path)/release

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
os                  ?= $(shell $(go_cmd) env GOOS)
arch                ?= $(shell $(go_cmd) env GOARCH)
multiplatform       ?= false
env                 ?= env GOOS=$(os) GOARCH=$(arch)
build_flags         ?= -buildvcs=false
release_build_flags ?= $(build_flags) -ldflags '-w -s'
test_cmd            ?= scripts/test-packages.sh
proto_flags         ?=

ifneq ("$(wildcard /usr/include)","")
    proto_flags += --proto_path="/usr/include"
endif

# Homebrew paths (Apple Silicon and Intel)
ifneq ("$(wildcard /opt/homebrew/include)","")
    proto_flags += --proto_path="/opt/homebrew/include"
endif

PYTHON_CMD ?= $(shell command -v python3 2>/dev/null || command -v python 2>/dev/null)

# Set additional parameter to build the test-node for different platforms and push
# E.g., make multiplatform=true docker_push=true build-image-test-node
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
REQUIRES_DB_PACKAGES_REGEXP = ${ROOT_PKG_REGEXP}/(service/coordinator|loadgen|cmd|utils/testdb)
HEAVY_PACKAGES_REGEXP = ${ROOT_PKG_REGEXP}/(docker|integration)

NON_HEAVY_PACKAGES=$(shell $(go_cmd) list ./... | grep -vE "$(HEAVY_PACKAGES_REGEXP)")
COR_DB_PACKAGES=$(shell $(go_cmd) list ./... | grep -E "$(CORE_DB_PACKAGES_REGEXP)")
REQUIRES_DB_PACKAGES=$(shell $(go_cmd) list ./... | grep -E "$(REQUIRES_DB_PACKAGES_REGEXP)")
NO_DB_PACKAGES=$(shell $(go_cmd) list ./... | grep -vE "$(CORE_DB_PACKAGES_REGEXP)|$(REQUIRES_DB_PACKAGES_REGEXP)|$(HEAVY_PACKAGES_REGEXP)")

# Excludes integration and container tests.
# Use `test-integration`, `test-integration-db-resiliency`, and `test-container`.
test: build
	@$(test_cmd) "${NON_HEAVY_PACKAGES}"

# Test a specific package.
test-package-%: build
	@$(test_cmd) ./$*/...

# Integration tests excluding DB resiliency tests.
# Use `test-integration-db-resiliency`.
test-integration: build
	@$(test_cmd) ./integration/... -skip "DBResiliency.*"

# DB resiliency integration tests.
test-integration-db-resiliency: build
	@$(test_cmd) ./integration/... -run "DBResiliency.*"

# Tests the all-in-one docker image.
test-container: build-image-test-node build-image-release
	@$(test_cmd) ./docker/...

# Tests for components that directly talk to the DB, where different DBs might affect behaviour.
test-core-db: FORCE
	@$(test_cmd) "${COR_DB_PACKAGES}"

# Tests for components that depend on the DB layer, but are agnostic to the specific DB used.
test-requires-db: FORCE
	@$(test_cmd) "${REQUIRES_DB_PACKAGES}"

# Tests that require no DB at all, e.g., pure logic, utilities
test-no-db: FORCE
	@$(test_cmd) "${NO_DB_PACKAGES}" -coverprofile=coverage.profile -coverpkg=./...

# Tests for components that depend on the DB layer, and ones that are agnostic to the specific DB used.
test-all-db: FORCE
	@$(test_cmd) "${REQUIRES_DB_PACKAGES} ${COR_DB_PACKAGES}" -coverprofile=coverage.profile -coverpkg=./...

# Runs test coverage analysis. It uses same tests that will be covered by the CI.
test-cover: FORCE
	@$(test_cmd) "${NO_DB_PACKAGES} ${REQUIRES_DB_PACKAGES} ${COR_DB_PACKAGES}" \
		-coverprofile=coverage.profile -coverpkg=./...
	@scripts/test-coverage-filter-files.sh

cover-report: FORCE
	$(go_cmd) tool cover -html=coverage.profile

clean: FORCE
	@rm -rf $(bin_path)
	@rm -rf $(release_path)
	@rm -rf $(BUILD_DIR)

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

# Run deliver benchmarks with added op/sec column.
bench-deliver: FORCE
	$(go_cmd) test ./utils/deliverorderer/... -bench "Benchmark.*" -run "^$$" | awk -f scripts/bench-tx-per-sec.awk

#########################
# Code Generation
#########################

PROTO_COMMON_PATH="$(shell $(env) $(go_cmd) list -m -f '{{.Dir}}' github.com/hyperledger/fabric-x-common)"

BUILD_DIR := .build

# Fabric protos cloning for lint-proto (msp/msp_config.proto dependency)
fabric_protos_tag ?= $(shell $(go_cmd) list -m -f '{{.Version}}' github.com/hyperledger/fabric-protos-go-apiv2)
FABRIC_PROTOS_REPO := https://github.com/hyperledger/fabric-protos.git
FABRIC_PROTOS_PATH := ${BUILD_DIR}/fabric-protos@${fabric_protos_tag}
FABRIC_PROTOS_SENTINEL := ${FABRIC_PROTOS_PATH}/.git

# Google APIs cloning for proto generation (google/api/annotations.proto dependency)
GOOGLE_PROTOS_REPO := https://github.com/googleapis/googleapis.git
GOOGLE_PROTOS_PATH := ${BUILD_DIR}/googleapis
GOOGLE_PROTOS_SENTINEL := ${GOOGLE_PROTOS_PATH}/.git

$(FABRIC_PROTOS_SENTINEL):
	@echo "Cloning fabric-protos@${fabric_protos_tag}..."
	@mkdir -p ${BUILD_DIR}
	@git -c advice.detachedHead=false clone --branch ${fabric_protos_tag} \
		--depth 1 ${FABRIC_PROTOS_REPO} ${FABRIC_PROTOS_PATH}

$(GOOGLE_PROTOS_SENTINEL):
	@echo "Cloning googleapis..."
	@mkdir -p ${BUILD_DIR}
	@rm -rf ${GOOGLE_PROTOS_PATH}
	@git -c advice.detachedHead=false clone --single-branch --depth 1 ${GOOGLE_PROTOS_REPO} ${GOOGLE_PROTOS_PATH}

proto: FORCE $(GOOGLE_PROTOS_SENTINEL) $(FABRIC_PROTOS_SENTINEL)
	@echo "Generating protobufs: $(shell find ${project_path}/api -name '*.proto' -print0 \
		| xargs -0 -n 1 dirname | xargs -n 1 basename | sort -u)"
	@protoc \
	  --go_out=paths=source_relative:. \
	  --go-grpc_out=. \
	  --go-grpc_opt=paths=source_relative \
	  --grpc-gateway_out=. \
	  --grpc-gateway_opt=paths=source_relative \
	  --proto_path="${project_path}" \
	  --proto_path="${PROTO_COMMON_PATH}" \
	  --proto_path="${GOOGLE_PROTOS_PATH}" \
	  --proto_path="${FABRIC_PROTOS_PATH}" \
	  ${proto_flags} \
	  ${project_path}/api/*/*.proto

lint-proto: FORCE $(GOOGLE_PROTOS_SENTINEL) $(FABRIC_PROTOS_SENTINEL)
	@echo "Running protobuf linters..."
	@api-linter \
		-I="${project_path}/api" \
		-I="${PROTO_COMMON_PATH}" \
		-I="${GOOGLE_PROTOS_PATH}" \
		-I="${FABRIC_PROTOS_PATH}" \
		--config .apilinter.yaml \
		--set-exit-status \
		--output-format github \
		$(shell find ${project_path}/api -name '*.proto' | sed 's|${project_path}/api/||')

# Generate testing mocks
mocks: FORCE
	@COUNTERFEITER_NO_GENERATE_WARNING=true go generate ./...

#########################
# Binaries
#########################

TRACKED_FILES ?= $(shell git ls-files)
CLI_TOOLS:=committer loadgen mock
BUILD_TARGETS=$(foreach tool,$(CLI_TOOLS),$(bin_dir)/$(tool))
BUILD_ARCH=$(arch) amd64 arm64 s390x
RELEASE_BUILD_TARGETS=$(foreach tool,$(CLI_TOOLS),$(foreach arch,$(BUILD_ARCH),$(release_dir)/linux-$(arch)/$(tool)))

# Helper function to produce the cross-compile env vars from a release/<os>-<arch>/<cmd> target stem.
release_env = CGO_ENABLED=0 \
	GOOS=$(word 1,$(subst -, ,$(notdir $(patsubst %/,%,$(dir $(1)))))) \
	GOARCH=$(word 2,$(subst -, ,$(notdir $(patsubst %/,%,$(dir $(1))))))

build: $(BUILD_TARGETS)

build-release: $(RELEASE_BUILD_TARGETS)

build-release-%: $(foreach arch,$(BUILD_ARCH),$(release_dir)/linux-$(arch)/%)
	@# This comment is required for the rule to work properly.

build-cmd-%: $(bin_dir)/%
	@# This comment is required for the rule to work properly.

build-image-%: $(BUILD_DIR)/%-image
	@# This comment is required for the rule to work properly.

$(bin_dir)/%: $(TRACKED_FILES)
	@mkdir -p "$(bin_path)"
	$(env) $(go_cmd) build $(build_flags) -o "$(bin_path)/$*" "./cmd/$*"

$(release_dir)/%: $(TRACKED_FILES)
	@mkdir -p $(release_path)/$(shell dirname $*)
	env $(call release_env,$*) $(go_cmd) build $(release_build_flags) -o "$(release_path)/$*" "./cmd/$(notdir $*)"

$(BUILD_DIR)/test-node-image: $(RELEASE_BUILD_TARGETS)
	${docker_cmd} build $(docker_build_flags) \
		-f $(dockerfile_test_node_path)/Dockerfile \
		-t ${image_namespace}/committer-test-node:${version} \
		--build-arg SRC_BIN_PATH=${release_dir} \
		. $(docker_push_arg)
	@mkdir -p ${BUILD_DIR}
	@touch $@

$(BUILD_DIR)/release-image: $(RELEASE_BUILD_TARGETS)
	./scripts/build-release-image.sh \
    	$(docker_cmd) $(version) $(image_namespace) $(dockerfile_release_path) $(multiplatform) $(release_dir)
	@mkdir -p ${BUILD_DIR}
	@touch $@

build-with-docker: FORCE
	@# Use the host local gocache and gomodcache folder to avoid rebuilding and re-downloading every time
	@mkdir -p "$(cache_path)" "$(mod_cache_path)" "$(bin_path)"
	@# We pass TRACKED_FILES from the host to avoid running git inside the container
    # which may fail with 'dubious ownership' errors when the repo is owned by a different user.
	@$(docker_cmd) run --rm -it \
	  --mount "type=bind,source=$(project_path),target=$(project_path)" \
	  --mount "type=bind,source=$(cache_path),target=$(cache_path)" \
	  --mount "type=bind,source=$(mod_cache_path),target=$(mod_cache_path)" \
	  --workdir $(project_path) \
	  --env GOCACHE="$(cache_path)" \
	  --env GOMODCACHE="$(mod_cache_path)" \
	  $(golang_image) \
      make build TRACKED_FILES="$(TRACKED_FILES)" bin_dir=$(bin_dir) env="$(env)"
	scripts/amend-permissions.sh "$(cache_path)" "$(mod_cache_path)"

#########################
# Linter
#########################

lint: check-metrics-doc lint-proto FORCE
	@echo "Running Go Linters..."
	golangci-lint run --color=always --new-from-rev=main --timeout=4m
	scripts/lint.sh $(go_cmd)
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
